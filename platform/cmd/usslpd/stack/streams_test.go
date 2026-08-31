package stack

import (
	"testing"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// canonicalPartitionTotal is the figure the devStreams comment argues from, and
// the figure deploy/terraform and docs/architecture/scalability.md size the MSK
// cluster against.
//
// It is asserted rather than left in prose because it drifted once: the comment
// claimed 5,568 partitions against a catalogue totalling 5,472, and nothing
// failed. A stream added to canon.AllStreams, or a partition count changed, now
// breaks this test and the reader is sent to the places that transcribe it.
const canonicalPartitionTotal = 5472

func TestCatalogueTotalMatchesTheCommentAndTheClusterSizing(t *testing.T) {
	total := 0
	for _, s := range canon.AllStreams() {
		total += s.Partitions
	}
	if total != canonicalPartitionTotal {
		t.Fatalf("canon.AllStreams() totals %d partitions, want %d.\n"+
			"If this is a deliberate catalogue change, update: the devStreams comment in "+
			"streams.go, deploy/terraform/modules/msk/main.tf and deploy/terraform/README.md, "+
			"deploy/compose/README.md, docs/architecture/scalability.md §2.3 and "+
			"docs/adr/0007-per-key-partition-ordering.md.", total, canonicalPartitionTotal)
	}
}

// The clamp is checked end to end by TestDevStreamsPreserveTheCatalogue, which
// needs a booted runtime and is therefore skipped in -short. This is the same
// property at the unit level, so a clamp that starts widening a stream — or
// rewriting a retention — fails on the fast suite too.
func TestDevStreamsClampsWithoutWidening(t *testing.T) {
	canonical := map[string]canon.Stream{}
	for _, s := range canon.AllStreams() {
		canonical[s.Name] = s
	}

	// 0 and -1 are a caller that has not configured a count; both must land on
	// the documented default rather than on a log with no partitions.
	for _, requested := range []int{DefaultDevPartitions, 0, -1} {
		got := devStreams(requested)
		if len(got) != len(canonical) {
			t.Fatalf("devStreams(%d) returned %d streams, want %d",
				requested, len(got), len(canonical))
		}
		for _, s := range got {
			want, ok := canonical[s.Name]
			if !ok {
				t.Fatalf("devStreams(%d) invented a stream %q that is not in the catalogue",
					requested, s.Name)
			}
			switch {
			case s.Partitions <= 0:
				t.Errorf("devStreams(%d): %s has %d partitions", requested, s.Name, s.Partitions)
			case s.Partitions > DefaultDevPartitions:
				t.Errorf("devStreams(%d): %s has %d partitions, above the clamp of %d",
					requested, s.Name, s.Partitions, DefaultDevPartitions)
			case s.Partitions > want.Partitions:
				t.Errorf("devStreams(%d): %s widened from the catalogue's %d to %d; "+
					"the clamp must only ever narrow", requested, s.Name, want.Partitions, s.Partitions)
			}
			// The clamp is a deployment-shape decision, not a schema change:
			// everything except the partition count has to be canon's.
			if s.RetentionHours != want.RetentionHours || s.Compacted != want.Compacted ||
				s.Description != want.Description {
				t.Errorf("devStreams(%d): %s had its schema altered, not just its partition count",
					requested, s.Name)
			}
		}
	}
}
