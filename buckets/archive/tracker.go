package archive

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	logger "github.com/ipfs/go-log"
	"github.com/textileio/go-threads/core/thread"
	powc "github.com/textileio/powergate/api/client"
	"github.com/textileio/powergate/ffs"
	bc "github.com/textileio/textile/buckets/collection"
	"github.com/textileio/textile/collections"
)

var (
	log = logger.Logger("pow-archive")

	checkInterval         = time.Second * 30
	jobStatusPollInterval = time.Second * 30
	maxConcurrent         = 20
)

type Tracker struct {
	lock   sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	closed chan (struct{})

	colls    *collections.Collections
	buckets  *bc.Buckets
	pgClient *powc.Client
}

func New(colls *collections.Collections, buckets *bc.Buckets, pgClient *powc.Client) (*Tracker, error) {
	ctx, cancel := context.WithCancel(context.Background())
	t := &Tracker{
		ctx:    ctx,
		cancel: cancel,
		closed: make(chan struct{}),

		colls:    colls,
		buckets:  buckets,
		pgClient: pgClient,
	}
	return t, nil
}

func (t *Tracker) Close() error {
	t.cancel()
	<-t.closed
	return nil
}

func (t *Tracker) run() {
	for {
		select {
		case <-t.ctx.Done():
			log.Info("shutting down archive tracker daemon")
			return
		case <-time.After(checkInterval):
			for {
				archives, err := t.colls.ArchiveTracking.GetReadyToCheck(t.ctx, maxConcurrent)
				if err != nil {
					log.Errorf("getting tracked archives: %s", err)
					break
				}
				if len(archives) == 0 {
					break
				}
				var wg sync.WaitGroup
				wg.Add(len(archives))
				for _, a := range archives {
					go func(a *collections.TrackedArchive) {
						defer wg.Done()

						ctx, cancel := context.WithTimeout(t.ctx, time.Second*5)
						defer cancel()
						reschedule, cause, err := t.trackArchiveProgress(ctx, a.BucketKey, a.DbID, a.DbToken, a.JID, a.BucketRoot)
						if err != nil || reschedule {
							if err != nil {
								cause = err.Error()
							}
							if err != nil {
								if err := t.colls.ArchiveTracking.Finalize(ctx, a.JID, cause); err != nil {
									log.Errorf("finalizing errored/rescheduled archive tracking: %s", err)
								}
							}
							return
						}
						if err := t.colls.ArchiveTracking.Reschedule(ctx, a.JID, jobStatusPollInterval); err != nil {
							log.Errorf("rescheduling tracked archive: %s", err)
						}

					}(a)
				}
				wg.Wait()
			}

		}
	}
}

func (t *Tracker) Track(ctx context.Context, dbID thread.ID, dbToken thread.Token, bucketKey string, jid ffs.JobID, bucketRoot cid.Cid) error {
	if err := t.colls.ArchiveTracking.Create(ctx, dbID, dbToken, bucketKey, jid, bucketRoot); err != nil {
		return fmt.Errorf("saving tracking information: %s", err)
	}
	return nil
}

// trackArcvhiveProgress queries the current archive status.
// If a fatal error in tracking happens, it will return an error, which indicates the archive should be untracked.
// If the archive didn't reach a final status yet, or a possibly recoverable error (by retrying) happens, it will return (true, "retry cause", nil).
// If the archive reach final status, it will return (false, "", nil) and the tracking can be considered done.
func (t *Tracker) trackArchiveProgress(ctx context.Context, buckKey string, dbID thread.ID, dbToken thread.Token, jid ffs.JobID, bucketRoot cid.Cid) (bool, string, error) {
	ffsi, err := t.colls.FFSInstances.Get(ctx, buckKey)
	if err != nil {
		return false, "", fmt.Errorf("getting ffs instance data: %s", err)
	}
	// Step 1: watch for the Job status, and keep updating on Mongo until
	// we're in a final state.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctx = context.WithValue(ctx, powc.AuthKey, ffsi.FFSToken)
	ch := make(chan powc.JobEvent, 1)
	if err := t.pgClient.FFS.WatchJobs(ctx, ch, jid); err != nil {
		return true, fmt.Sprintf("watching current job %s for bucket %s: %s", jid, buckKey, err), nil
	}

	var aborted bool
	var abortMsg string
	var job ffs.Job
	select {
	case <-ctx.Done():
		log.Infof("job %s status watching canceled", jid)
		return true, "watching cancelled", nil
	case s, ok := <-ch:
		if !ok {
			log.Errorf("getting job %s status stream closed", jid)
			aborted = true
			abortMsg = "server closed communication channel"
		}
		if s.Err != nil {
			log.Errorf("job %s update: %s", jid, s.Err)
			aborted = true
			abortMsg = s.Err.Error()
		}
		job = s.Job
	}

	if !isJobStatusFinal(job.Status) {
		return true, "No final status yet", nil
	}

	if err := t.updateArchiveStatus(ctx, ffsi, job, aborted, abortMsg); err != nil {
		return true, fmt.Sprintf("updating archive status: %s", err), nil
	}

	// Step 2: On success, save Deal data in the underlying Bucket thread. On
	// failure save the error message.
	if job.Status == ffs.Success {
		if err := t.saveDealsInArchive(ctx, buckKey, dbID, dbToken, ffsi.FFSToken, bucketRoot); err != nil {
			return true, fmt.Sprintf("saving deal data in archive: %s", err), nil
		}
	}
	return false, "", nil
}

// updateArchiveStatus save the last known job status. It also receives an
// _abort_ flag which indicates that the provided newJobStatus might not be
// final. For example, if tracking the Job status changes errored by some network
// condition, we have only the last known Job status. Powergate would continue doing
// its work until a final status is reached; it only means we aren't sure how this
// archive really finished.
// To track for this situation, we use the _aborted_ and _abortMsg_ parameters.
// An archive with _aborted_ true should eventually be re-queried to understand
// how it finished (if wanted).
func (t *Tracker) updateArchiveStatus(ctx context.Context, ffsi *collections.FFSInstance, job ffs.Job, aborted bool, abortMsg string) error {
	t.lock.Lock()
	defer t.lock.Unlock()
	lastArchive := &ffsi.Archives.Current
	if lastArchive.JobID != job.ID.String() {
		for i := range ffsi.Archives.History {
			if ffsi.Archives.History[i].JobID == job.ID.String() {
				lastArchive = &ffsi.Archives.History[i]
				break
			}
		}
	}
	lastArchive.JobStatus = int(job.Status)
	lastArchive.Aborted = aborted
	lastArchive.AbortedMsg = abortMsg
	lastArchive.FailureMsg = prepareFailureMsg(job)
	if err := t.colls.FFSInstances.Replace(ctx, ffsi); err != nil {
		return fmt.Errorf("updating ffs status update instance data: %s", err)
	}
	return nil
}

func (t *Tracker) saveDealsInArchive(ctx context.Context, key string, dbID thread.ID, dbToken thread.Token, ffsToken string, c cid.Cid) error {
	opts := bc.WithToken(dbToken)
	buck, err := t.buckets.Get(ctx, dbID, key, opts)
	if err != nil {
		return fmt.Errorf("getting bucket for save deals: %s", err)
	}
	ctxFFS := context.WithValue(ctx, powc.AuthKey, ffsToken)
	sh, err := t.pgClient.FFS.Show(ctxFFS, c)
	if err != nil {
		return fmt.Errorf("getting cid info: %s", err)
	}

	proposals := sh.GetCidInfo().GetCold().GetFilecoin().GetProposals()

	deals := make([]bc.Deal, len(proposals))
	for i, p := range proposals {
		deals[i] = bc.Deal{
			ProposalCid: p.GetProposalCid(),
			Miner:       p.GetMiner(),
		}
	}
	buck.Archives.Current = bc.Archive{
		Cid:   c.String(),
		Deals: deals,
	}

	buck.UpdatedAt = time.Now().UnixNano()
	if err = t.buckets.Save(ctx, dbID, buck, opts); err != nil {
		return fmt.Errorf("saving deals in thread: %s", err)
	}
	return nil
}

func prepareFailureMsg(job ffs.Job) string {
	if job.ErrCause == "" {
		return ""
	}
	var b strings.Builder
	_, _ = b.WriteString(job.ErrCause)
	for i, de := range job.DealErrors {
		_, _ = b.WriteString(fmt.Sprintf("\nDeal error %d: Proposal %s with miner %s, %s", i, de.ProposalCid, de.Miner, de.Message))
	}
	return b.String()
}

func isJobStatusFinal(js ffs.JobStatus) bool {
	return js == ffs.Success ||
		js == ffs.Canceled ||
		js == ffs.Failed
}
