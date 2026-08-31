package e2e

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/usslp/usslp/platform/cmd/usslpd/stack"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// contractPath is the normative document every number in this suite is measured
// against.
const contractPath = "../../docs/architecture/INTERFACE-CONTRACTS.md"

// TestLatencyBudgetMatchesTheContract keeps the budget table the runtime reports
// against in step with the document that defines it.
//
// stack.Budget is a transcription, and a transcription that drifts is worse than
// no transcription: the suite would keep reporting "within budget" against a
// budget nobody agreed to. Parsing §4 out of the Markdown is ugly and it is the
// only thing that actually catches the drift.
func TestLatencyBudgetMatchesTheContract(t *testing.T) {
	body, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("reading the interface contract: %v", err)
	}
	doc := string(body)

	// The budget lives in a fenced block in section 4, one hop per line:
	//	POS → UIG            ≤  50 ms   validate, dedupe, normalise, publish
	line := regexp.MustCompile(`(?m)^(.+?)\s+≤\s*(\d+)\s*ms\s`)
	matches := line.FindAllStringSubmatch(doc, -1)
	if len(matches) == 0 {
		t.Fatalf("found no hop budgets in %s; has section 4 changed shape?", contractPath)
	}

	documented := map[string]int64{}
	var total int64
	for _, m := range matches {
		name := normaliseHop(m[1])
		ms, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			t.Fatalf("parsing the budget for %q: %v", name, err)
		}
		documented[name] = ms
		total += ms
	}

	if len(documented) != len(stack.Budget) {
		t.Errorf("the contract lists %d hops and stack.Budget has %d",
			len(documented), len(stack.Budget))
	}
	for _, hop := range stack.Budget {
		want, ok := documented[normaliseHop(hop.Name)]
		if !ok {
			t.Errorf("stack.Budget has a hop %q that the contract does not: %v",
				hop.Name, keysOf(documented))
			continue
		}
		if want != hop.BudgetMS {
			t.Errorf("hop %q: the contract says %d ms, stack.Budget says %d ms",
				hop.Name, want, hop.BudgetMS)
		}
	}
	if total != stack.BudgetTotalMS() {
		t.Errorf("the contract's hops sum to %d ms, stack.Budget's to %d ms",
			total, stack.BudgetTotalMS())
	}
	if stack.BudgetTotalMS() != stack.TotalBudget.Milliseconds() {
		t.Errorf("the hop budgets sum to %d ms but the end-to-end SLO is %d ms",
			stack.BudgetTotalMS(), stack.TotalBudget.Milliseconds())
	}
	t.Logf("%d hops summing to %d ms, matching %s", len(stack.Budget), total, contractPath)
}

// normaliseHop reduces a hop name to something comparable across an arrow
// character, a code font and a change of spacing.
func normaliseHop(s string) string {
	s = strings.ReplaceAll(s, "→", "->")
	s = strings.ReplaceAll(s, "`", "")
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	// The document abbreviates two components; the runtime spells them out.
	s = strings.ReplaceAll(s, "label svc", "label service")
	return s
}

func keysOf(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestDevStreamsPreserveTheCatalogue checks the one thing usslpd deliberately
// changes about the platform's configuration.
//
// The partition counts are overridden for a laptop, and that is defensible
// because ordering in USSLP is per partition key rather than per partition
// count. Everything else about the catalogue — the names, the retentions, the
// compaction flags, the set of streams — must be canon's, because a store
// gateway later re-pointed at a real Kafka cluster has to find the catalogue it
// expects.
func TestDevStreamsPreserveTheCatalogue(t *testing.T) {
	canonical := canon.AllStreams()
	byName := map[string]canon.Stream{}
	for _, s := range canonical {
		byName[s.Name] = s
	}

	dev := shared.EventLog().Streams()
	if len(dev) != len(canonical) {
		t.Errorf("the runtime provisioned %d streams, the catalogue has %d", len(dev), len(canonical))
	}
	for _, got := range dev {
		want, ok := byName[got.Name]
		if !ok {
			t.Errorf("the runtime provisioned a stream %q that is not in canon.AllStreams()", got.Name)
			continue
		}
		if got.RetentionHours != want.RetentionHours {
			t.Errorf("%s: retention is %dh, the catalogue says %dh",
				got.Name, got.RetentionHours, want.RetentionHours)
		}
		if got.Compacted != want.Compacted {
			t.Errorf("%s: compaction is %v, the catalogue says %v",
				got.Name, got.Compacted, want.Compacted)
		}
		if got.Partitions > want.Partitions {
			t.Errorf("%s: the runtime widened the stream to %d partitions from the catalogue's %d",
				got.Name, got.Partitions, want.Partitions)
		}
		if got.Partitions <= 0 {
			t.Errorf("%s: %d partitions", got.Name, got.Partitions)
		}
	}
	t.Logf("%d streams provisioned with names, retentions and compaction from canon.AllStreams(), "+
		"partitions clamped to %d", len(dev), shared.Config().DevPartitions)
}
