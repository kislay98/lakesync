package mongodb

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"lakesync/internal/source"
)

// OplogInfo estimates the oplog window from the oldest and newest entries of
// local.oplog.rs (stretch goal S4). It needs read access to the local database,
// which Atlas grants to users with the `read` role on local or to admins; when
// the privilege is missing the error says so and the UI hides the estimate.
func (s *Source) OplogInfo(ctx context.Context) (*source.OplogInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	coll := s.client.Database("local").Collection("oplog.rs")
	var first, last struct {
		TS bson.Timestamp `bson:"ts"`
	}
	if err := coll.FindOne(ctx, bson.D{}, options.FindOne().SetSort(bson.D{{Key: "$natural", Value: 1}}).SetProjection(bson.D{{Key: "ts", Value: 1}})).Decode(&first); err != nil {
		return nil, fmt.Errorf("read the oldest oplog entry (needs read access to the local database): %w", err)
	}
	if err := coll.FindOne(ctx, bson.D{}, options.FindOne().SetSort(bson.D{{Key: "$natural", Value: -1}}).SetProjection(bson.D{{Key: "ts", Value: 1}})).Decode(&last); err != nil {
		return nil, fmt.Errorf("read the newest oplog entry: %w", err)
	}
	info := &source.OplogInfo{Oldest: time.Unix(int64(first.TS.T), 0).UTC(), Newest: time.Unix(int64(last.TS.T), 0).UTC()}
	info.WindowSeconds = info.Newest.Sub(info.Oldest).Seconds()
	var stats struct {
		Size int64 `bson:"size"`
	}
	if err := s.client.Database("local").RunCommand(ctx, bson.D{{Key: "collStats", Value: "oplog.rs"}}).Decode(&stats); err == nil {
		info.SizeBytes = stats.Size
	}
	return info, nil
}
