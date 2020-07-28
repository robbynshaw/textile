package collection

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/jsonschema"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log"
	"github.com/ipfs/interface-go-ipfs-core/path"
	dbc "github.com/textileio/go-threads/api/client"
	"github.com/textileio/go-threads/core/thread"
	db "github.com/textileio/go-threads/db"
	tutil "github.com/textileio/go-threads/util"
	powc "github.com/textileio/powergate/api/client"
	"github.com/textileio/powergate/ffs"
	"github.com/textileio/textile/buckets"
	"github.com/textileio/textile/collections"
)

var (
	log = logging.Logger("buckets")

	schema  *jsonschema.Schema
	indexes = []db.Index{{
		Path: "path",
	}}

	// ffsDefaultStorageConfig is a default hardcoded CidConfig to be used
	// on newly created FFS instances as the default CidConfig of archived Cids,
	// if none is provided in constructor.
	ffsDefaultStorageConfig = ffs.StorageConfig{
		Hot: ffs.HotConfig{
			Enabled:       false,
			AllowUnfreeze: true,
			Ipfs: ffs.IpfsConfig{
				AddTimeout: 60 * 2,
			},
		},
		Cold: ffs.ColdConfig{
			Enabled: true,
			Filecoin: ffs.FilConfig{
				RepFactor:       10,     // Aim high for testnet
				DealMinDuration: 200000, // ~2 months
			},
		},
	}
)

// Bucket represents the buckets threaddb collection schema.
type Bucket struct {
	Key       string   `json:"_id"`
	Name      string   `json:"name"`
	Path      string   `json:"path"`
	EncKey    string   `json:"key,omitempty"`
	DNSRecord string   `json:"dns_record,omitempty"`
	Archives  Archives `json:"archives"`
	CreatedAt int64    `json:"created_at"`
	UpdatedAt int64    `json:"updated_at"`
}

// GetEncKey returns the encryption key as bytes if present.
func (b *Bucket) GetEncKey() []byte {
	if b.EncKey == "" {
		return nil
	}
	key, _ := base64.StdEncoding.DecodeString(b.EncKey)
	return key
}

// Archives contains all archives for a single bucket.
type Archives struct {
	Current Archive   `json:"current"`
	History []Archive `json:"history"`
}

// Archive is a single archive containing a list of deals.
type Archive struct {
	Cid   string `json:"cid"`
	Deals []Deal `json:"deals"`
}

// Deal contains details about a Filecoin deal.
type Deal struct {
	ProposalCid string `json:"proposal_cid"`
	Miner       string `json:"miner"`
}

func init() {
	reflector := jsonschema.Reflector{ExpandedStruct: true}
	schema = reflector.Reflect(&Bucket{})
}

// Buckets is a wrapper around a threaddb collection that performs object storage on IPFS and Filecoin.
type Buckets struct {
	ffsCol   *collections.FFSInstances
	threads  *dbc.Client
	pgClient *powc.Client

	buckCidConfig ffs.StorageConfig

	lock   sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New returns a new buckets collection mananger.
func New(t *dbc.Client, pgc *powc.Client, col *collections.FFSInstances, defaultCidConfig *ffs.StorageConfig, debug bool) (*Buckets, error) {
	if debug {
		if err := tutil.SetLogLevels(map[string]logging.LogLevel{
			"buckets": logging.LevelDebug,
		}); err != nil {
			return nil, err
		}
	}
	buckCidConfig := ffsDefaultStorageConfig
	if defaultCidConfig != nil {
		buckCidConfig = *defaultCidConfig
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Buckets{
		ffsCol:   col,
		threads:  t,
		pgClient: pgc,

		buckCidConfig: buckCidConfig,

		ctx:    ctx,
		cancel: cancel,
	}, nil
}

// Create a bucket instance.
func (b *Buckets) Create(ctx context.Context, dbID thread.ID, key string, pth path.Path, opts ...Option) (*Bucket, error) {
	args := &Options{}
	for _, opt := range opts {
		opt(args)
	}
	var encKey string
	if args.Key != nil {
		encKey = base64.StdEncoding.EncodeToString(args.Key)
	}
	now := time.Now().UnixNano()
	bucket := &Bucket{
		Key:       key,
		Name:      args.Name,
		Path:      pth.String(),
		EncKey:    encKey,
		Archives:  Archives{Current: Archive{Deals: []Deal{}}, History: []Archive{}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	ids, err := b.threads.Create(ctx, dbID, buckets.CollectionName, dbc.Instances{bucket}, db.WithTxnToken(args.Token))
	if isColNotFoundErr(err) {
		if err := b.addCollection(ctx, dbID, opts...); err != nil {
			return nil, err
		}
		return b.Create(ctx, dbID, key, pth, opts...)
	}
	if isInvalidSchemaErr(err) {
		if err := b.updateCollection(ctx, dbID, opts...); err != nil {
			return nil, err
		}
		return b.Create(ctx, dbID, key, pth, opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("creating bucket in thread: %s", err)
	}
	bucket.Key = ids[0]

	if err := b.createFFSInstance(ctx, key); err != nil {
		return nil, fmt.Errorf("creating FFS instance for bucket: %s", err)
	}

	return bucket, nil
}

// IsArchivingEnabled returns whether or not Powergate archiving is enabled.
func (b *Buckets) IsArchivingEnabled() bool {
	return b.pgClient != nil
}

func (b *Buckets) createFFSInstance(ctx context.Context, bucketKey string) error {
	// If the Powergate client isn't configured, don't do anything.
	if b.pgClient == nil {
		return nil
	}
	_, token, err := b.pgClient.FFS.Create(ctx)
	if err != nil {
		return fmt.Errorf("creating FFS instance: %s", err)
	}

	ctxFFS := context.WithValue(ctx, powc.AuthKey, token)
	i, err := b.pgClient.FFS.Info(ctxFFS)
	if err != nil {
		return fmt.Errorf("getting information about created ffs instance: %s", err)
	}
	waddr := i.Balances[0].Addr
	if err := b.ffsCol.Create(ctx, bucketKey, token, waddr); err != nil {
		return fmt.Errorf("saving FFS instances data: %s", err)
	}
	defaultBucketCidConfig := ffs.StorageConfig{
		Cold:       b.buckCidConfig.Cold,
		Hot:        b.buckCidConfig.Hot,
		Repairable: b.buckCidConfig.Repairable,
	}
	defaultBucketCidConfig.Cold.Filecoin.Addr = waddr
	if err := b.pgClient.FFS.SetDefaultStorageConfig(ctxFFS, defaultBucketCidConfig); err != nil {
		return fmt.Errorf("setting default bucket FFS cidconfig: %s", err)
	}
	return nil
}

// Get a bucket instance.
func (b *Buckets) Get(ctx context.Context, dbID thread.ID, key string, opts ...Option) (*Bucket, error) {
	args := &Options{}
	for _, opt := range opts {
		opt(args)
	}
	buck := &Bucket{}
	err := b.threads.FindByID(ctx, dbID, buckets.CollectionName, key, buck, db.WithTxnToken(args.Token))
	if isColNotFoundErr(err) {
		if err := b.addCollection(ctx, dbID, opts...); err != nil {
			return nil, err
		}
		return b.Get(ctx, dbID, key, opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("getting bucket in thread: %s", err)
	}
	return buck, nil
}

// List bucket instances.
func (b *Buckets) List(ctx context.Context, dbID thread.ID, opts ...Option) ([]*Bucket, error) {
	args := &Options{}
	for _, opt := range opts {
		opt(args)
	}
	res, err := b.threads.Find(ctx, dbID, buckets.CollectionName, &db.Query{}, &Bucket{}, db.WithTxnToken(args.Token))
	if isColNotFoundErr(err) {
		if err := b.addCollection(ctx, dbID, opts...); err != nil {
			return nil, err
		}
		return b.List(ctx, dbID, opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("listing bucket in thread: %s", err)
	}
	return res.([]*Bucket), nil
}

// Save a bucket instance.
func (b *Buckets) Save(ctx context.Context, dbID thread.ID, bucket *Bucket, opts ...Option) error {
	args := &Options{}
	for _, opt := range opts {
		opt(args)
	}
	ensureNoNulls(bucket)
	err := b.threads.Save(ctx, dbID, buckets.CollectionName, dbc.Instances{bucket}, db.WithTxnToken(args.Token))
	if isInvalidSchemaErr(err) {
		if err := b.updateCollection(ctx, dbID, opts...); err != nil {
			return err
		}
		return b.Save(ctx, dbID, bucket, opts...)
	}
	if err != nil {
		return fmt.Errorf("saving bucket in thread: %s", err)
	}
	return nil

}

func ensureNoNulls(b *Bucket) {
	if len(b.Archives.History) == 0 {
		current := b.Archives.Current
		if len(current.Deals) == 0 {
			b.Archives.Current = Archive{Deals: []Deal{}}
		}
		b.Archives = Archives{Current: current, History: []Archive{}}
	}
}

// Delete a bucket instance.
func (b *Buckets) Delete(ctx context.Context, dbID thread.ID, key string, opts ...Option) error {
	args := &Options{}
	for _, opt := range opts {
		opt(args)
	}
	err := b.threads.Delete(ctx, dbID, buckets.CollectionName, []string{key}, db.WithTxnToken(args.Token))
	if err != nil {
		return fmt.Errorf("deleting bucket in thread: %s", err)
	}
	return nil
}

// ArchiveStatus returns the last known archive status on Powergate. If the return status is Failed,
// an extra string with the error message is provided.
func (b *Buckets) ArchiveStatus(ctx context.Context, key string) (ffs.JobStatus, string, error) {
	ffsi, err := b.ffsCol.Get(ctx, key)
	if err != nil {
		return ffs.Failed, "", fmt.Errorf("getting ffs instance data: %s", err)
	}

	if ffsi.Archives.Current.JobID == "" {
		return ffs.Failed, "", buckets.ErrNoCurrentArchive
	}
	current := ffsi.Archives.Current
	if current.Aborted {
		return ffs.Failed, "", fmt.Errorf("job status tracking was aborted: %s", current.AbortedMsg)
	}
	return ffs.JobStatus(current.JobStatus), current.FailureMsg, nil
}

// ArchiveWatch allows to have the last log execution for the last archive, plus realtime
// human-friendly log output of how the current archive is executing.
// If the last archive is already done, it will simply return the log history and close the channel.
func (b *Buckets) ArchiveWatch(ctx context.Context, key string, ch chan<- string) error {
	ffsi, err := b.ffsCol.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("getting ffs instance data: %s", err)
	}

	if ffsi.Archives.Current.JobID == "" {
		return buckets.ErrNoCurrentArchive
	}
	current := ffsi.Archives.Current
	if current.Aborted {
		return fmt.Errorf("job status tracking was aborted: %s", current.AbortedMsg)
	}
	c, err := cid.Cast(current.Cid)
	if err != nil {
		return fmt.Errorf("parsing current archive cid: %s", err)
	}
	ctx = context.WithValue(ctx, powc.AuthKey, ffsi.FFSToken)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ffsCh := make(chan powc.LogEvent)
	if err := b.pgClient.FFS.WatchLogs(ctx, ffsCh, c, powc.WithJidFilter(ffs.JobID(current.JobID)), powc.WithHistory(true)); err != nil {
		return fmt.Errorf("watching log events in Powergate: %s", err)
	}
	for le := range ffsCh {
		if le.Err != nil {
			return le.Err
		}
		ch <- le.LogEntry.Msg
	}
	return nil
}

func (b *Buckets) Close() error {
	b.cancel()
	b.wg.Wait()
	return nil
}

func (b *Buckets) addCollection(ctx context.Context, dbID thread.ID, opts ...Option) error {
	args := &Options{}
	for _, opt := range opts {
		opt(args)
	}
	return b.threads.NewCollection(ctx, dbID, db.CollectionConfig{
		Name:    buckets.CollectionName,
		Schema:  schema,
		Indexes: indexes,
	}, db.WithManagedToken(args.Token))
}

func (b *Buckets) updateCollection(ctx context.Context, dbID thread.ID, opts ...Option) error {
	args := &Options{}
	for _, opt := range opts {
		opt(args)
	}
	return b.threads.UpdateCollection(ctx, dbID, db.CollectionConfig{
		Name:    buckets.CollectionName,
		Schema:  schema,
		Indexes: indexes,
	}, db.WithManagedToken(args.Token))
}

type Options struct {
	Name  string
	Key   []byte
	Token thread.Token
}

type Option func(*Options)

func WithName(n string) Option {
	return func(args *Options) {
		args.Name = n
	}
}

func WithKey(k []byte) Option {
	return func(args *Options) {
		args.Key = k
	}
}

func WithToken(t thread.Token) Option {
	return func(args *Options) {
		args.Token = t
	}
}

func isColNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "collection not found")
}

func isInvalidSchemaErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "instance doesn't correspond to schema: (root)")
}
