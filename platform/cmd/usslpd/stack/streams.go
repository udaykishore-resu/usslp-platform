package stack

import "github.com/usslp/usslp/platform/pkg/canon"

// devStreams is canon.AllStreams() with every partition count clamped.
//
// # Why the override exists
//
// canon's counts are sized from the estate capacity model: 1,024 partitions on
// `price-updates` so that 52,000 updates per second spread across a consumer
// group of two hundred nodes, 2,048 on `label-telemetry` for 167,000 readings
// per second at fifty million labels. Those are the right numbers for the
// cluster the platform is sold to run on, and deploy/terraform provisions
// exactly them.
//
// They are the wrong numbers for one process. pkg/eventlog materialises a
// partition as a directory holding a segment file and a sparse index, so the
// full catalogue is 5,472 partitions: 5,472 directories, at least 10,944 files,
// and — because a consumer group with one member is assigned every partition —
// 5,472 offset entries to plan, commit and poll on every read cycle. Creating
// them costs seconds of syscalls on a laptop and roughly a hundred megabytes of
// inodes before a single price has been published, and polling them costs more
// CPU than the store generates in work.
//
// # Why clamping is safe
//
// The ordering guarantee in INTERFACE-CONTRACTS §2 is per partition *key*, not
// per partition count: `price-updates` is keyed `store:sku`, so two changes to
// the same product in the same store land on the same partition and stay
// ordered whether there are four partitions or a thousand. Partition count buys
// parallelism, and parallelism is what a single-process deployment does not
// need. Four is chosen so that the partition-assignment code path is genuinely
// exercised — a single-partition log would let a rebalancing bug hide — while
// staying small enough to be free.
//
// A stream whose canonical count is already at or below the clamp keeps it, so
// nothing is ever *widened* here.
//
// This is a deployment-shape decision and not a schema change: the stream
// names, retentions and compaction flags are canon's, and a store gateway that
// is later re-pointed at a real Kafka cluster finds the catalogue it expects.
func devStreams(partitions int) []canon.Stream {
	if partitions <= 0 {
		partitions = DefaultDevPartitions
	}
	all := canon.AllStreams()
	out := make([]canon.Stream, 0, len(all))
	for _, s := range all {
		if s.Partitions > partitions {
			s.Partitions = partitions
		}
		out = append(out, s)
	}
	return out
}
