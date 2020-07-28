package collections

import (
	"context"
	"fmt"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/textileio/go-threads/core/thread"
	"github.com/textileio/powergate/ffs"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

type TrackedArchive struct {
	JID        ffs.JobID    `bson:"_id"`
	DbID       thread.ID    `bson:"db_id"`
	DbToken    thread.Token `bson:"db_token"`
	BucketKey  string       `bson:"bucket_key"`
	BucketRoot cid.Cid      `bson:"bucket_key"`
	ReadyAt    time.Time    `bson:"ready_at"`
	ErrorCause string       `bson:"error_cause"`
	Active     bool         `bson:"active"`
}

type ArchiveTracking struct {
	col *mongo.Collection
}

func NewArchiveTracking(ctx context.Context, db *mongo.Database) (*ArchiveTracking, error) {
	s := &ArchiveTracking{
		col: db.Collection("archivetrackings"),
	}
	return s, nil
}

func (at *ArchiveTracking) GetReadyToCheck(ctx context.Context, n int) ([]*TrackedArchive, error) {
	filter := bson.M{"ready_at": bson.M{"$lte": time.Now()}, "active": false}
	cursor, err := at.col.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("querying ready tracked archives: %s", err)
	}
	defer cursor.Close(ctx)
	var tas []*TrackedArchive
	for cursor.Next(ctx) {
		var ta TrackedArchive
		if err := cursor.Decode(&ta); err != nil {
			return nil, err
		}
		tas = append(tas, &ta)
	}
	if err := cursor.Err(); err != nil {
		return nil, err
	}
	return tas, nil
}

func (at *ArchiveTracking) Finalize(ctx context.Context, jid ffs.JobID, cause string) error {
	res, err := at.col.UpdateOne(ctx, bson.M{"_id": jid}, bson.M{
		"$set": bson.M{
			"active":      true,
			"error_cause": cause,
		},
	})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return nil
}

func (at *ArchiveTracking) Reschedule(ctx context.Context, jid ffs.JobID, dur time.Duration) error {
	readyAt := time.Now().Add(dur)
	res, err := at.col.UpdateOne(ctx, bson.M{"_id": jid}, bson.M{
		"$set": bson.M{
			"ready_at": readyAt,
		},
	})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return nil

}
