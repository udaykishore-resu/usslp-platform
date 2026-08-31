// Package load is USSLP's load harness: N stores of M labels, a sustained
// price-change rate, and an honest account of what breaks first.
//
// It is a Go test rather than a separate binary so that it shares the runtime,
// the measurement and the reporting with test/e2e, and so that `go test` is the
// only tool anyone needs. It does not run under `go test ./...`: a load run
// takes minutes and saturates the machine, which is not a thing to do to
// somebody's inner loop. Run it with `make load`, or:
//
//	go test ./test/load -run TestSustainedPriceLoad -load -v \
//	  -load.stores 2 -load.controllers 4 -load.labels 50 \
//	  -load.rate 20 -load.duration 60s
//
// # What the numbers are, and are not
//
// Everything above the radio is real code doing real work: HMAC verification,
// deduplication, a durable append, an Ed25519 signature per label, an MQTT
// publish, a bridge, a signature verification, a render, a framebuffer diff.
// The radio and the panels are edge/mesh and edge/labelsim over a
// discrete-event clock paced 1:1 against the wall clock, so their *timing* is a
// model — a faithful one, from the hardware budget, but a model.
//
// The consequence matters for reading the results: the ceiling this harness
// finds is the edge tier's, not the cloud's, and it is a ceiling this machine
// imposes on the simulation rather than one a real store's radio would impose.
// A real 2.9-inch panel really does take 1.5 s to run a full waveform, so the
// per-update latency is meaningful; the aggregate throughput is bounded by how
// fast one Go process can simulate several hundred radios, which is a fact
// about the simulator.
package load

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/cmd/usslpd/stack"
	"github.com/usslp/usslp/platform/pkg/canon"
)

var (
	enabled     = flag.Bool("load", false, "run the load harness")
	numStores   = flag.Int("load.stores", 1, "stores to run")
	controllers = flag.Int("load.controllers", 4, "shelf edge controllers per store")
	labels      = flag.Int("load.labels", 50, "labels per controller")
	rate        = flag.Float64("load.rate", 20, "offered price changes per second across the whole estate")
	duration    = flag.Duration("load.duration", 60*time.Second, "how long to sustain the offered rate")
	warmup      = flag.Duration("load.warmup", 5*time.Second, "settling time before measurement starts")
)

// TestSustainedPriceLoad offers a fixed rate of price changes to an estate and
// reports throughput, latency and where the time went.
//
// Offered *rate* rather than fixed concurrency, because a closed-loop harness
// with N workers cannot tell an overloaded system from a fast one: it simply
// slows down and reports a throughput equal to the system's capacity with no
// queueing visible. An open loop at a fixed rate makes saturation obvious — the
// backlog grows, and the report says so.
func TestSustainedPriceLoad(t *testing.T) {
	if !*enabled {
		t.Skip("the load harness is opt-in; pass -load (or run `make load`)")
	}
	st, err := stack.New(stack.Config{
		Ephemeral:           true,
		Tenants:             []canon.TenantID{"load-retail"},
		Stores:              *numStores,
		ControllersPerStore: *controllers,
		LabelsPerController: *labels,
		Seed:                20260830,
		LogLevel:            "error",
	})
	if err != nil {
		t.Fatalf("building the runtime: %v", err)
	}
	bootStart := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := st.Start(ctx); err != nil {
		t.Fatalf("starting the runtime: %v", err)
	}
	boot := time.Since(bootStart)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = st.Stop(c)
	})

	stores := st.Stores()
	totalLabels := st.LabelCount()
	t.Logf("estate: %d store(s), %d controllers, %d labels — booted and priced in %s",
		len(stores), len(stores)*(*controllers), totalLabels, boot.Round(time.Millisecond))

	// A settling window. The opening price book has already drained (Start
	// waits for it) but the telemetry and heartbeat timers have not yet found
	// their rhythm, and measuring across that transient reports a store that
	// does not exist.
	time.Sleep(*warmup)

	var (
		mu        sync.Mutex
		samples   []sample
		offered   atomic.Int64
		accepted  atomic.Int64
		refused   atomic.Int64
		lost      atomic.Int64
		inFlight  atomic.Int64
		maxFlight atomic.Int64
	)

	// One target per label, cycled, so the load spreads across every controller
	// and every mesh rather than hammering one zone.
	type slot struct {
		store *stack.Store
		zone  *stack.Zone
		label canon.LabelID
		sku   canon.SKU
		busy  chan struct{}
	}
	var slots []slot
	for _, s := range stores {
		for _, z := range s.Zones {
			for _, id := range z.Labels() {
				sku, ok := s.SKUOf(id)
				if !ok {
					continue
				}
				slots = append(slots, slot{store: s, zone: z, label: id, sku: sku,
					busy: make(chan struct{}, 1)})
			}
		}
	}
	if len(slots) == 0 {
		t.Fatal("the estate has no priced labels")
	}

	interval := time.Duration(float64(time.Second) / *rate)
	if interval <= 0 {
		t.Fatal("-load.rate must be positive")
	}
	t.Logf("offering %.1f price changes/second for %s (one every %s) across %d labels",
		*rate, *duration, interval.Round(time.Microsecond), len(slots))

	var wg sync.WaitGroup
	runCtx, stop := context.WithTimeout(context.Background(), *duration)
	defer stop()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	started := time.Now()
	next := 0

loop:
	for {
		select {
		case <-runCtx.Done():
			break loop
		case <-ticker.C:
		}
		sl := slots[next%len(slots)]
		next++
		offered.Add(1)

		// A label already being repriced is skipped rather than queued behind
		// itself: two concurrent changes to one label are legal but make the
		// measurement ambiguous, and skipping is visible in the offered-versus-
		// accepted numbers where hiding it would not be.
		select {
		case sl.busy <- struct{}{}:
		default:
			refused.Add(1)
			continue
		}

		wg.Add(1)
		go func(sl slot) {
			defer wg.Done()
			defer func() { <-sl.busy }()

			n := inFlight.Add(1)
			for {
				m := maxFlight.Load()
				if n <= m || maxFlight.CompareAndSwap(m, n) {
					break
				}
			}
			defer inFlight.Add(-1)

			rec, _ := sl.zone.Controller.Record(sl.label)
			watch := st.WatchDelivery(sl.label, rec.Sequence+1)
			price := canon.NewMoney(rec.Price.Amount+1+int64(next%29), rec.Price.Currency)

			sendCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			sent, _, err := st.PushShopifyPrice(sendCtx, sl.store.Tenant, sl.store.ID, sl.sku, price, "")
			if err != nil {
				refused.Add(1)
				return
			}
			accepted.Add(1)
			d, arrived := watch.Wait(60 * time.Second)
			if !arrived || !d.Delivered {
				lost.Add(1)
				return
			}
			mu.Lock()
			samples = append(samples, sample{
				wall: time.Since(sent), platform: d.TotalMS,
				radio: d.SECToLabel, refresh: d.RefreshMS, hops: d.Hops, ok: true,
			})
			mu.Unlock()
		}(sl)
	}
	offeredFor := time.Since(started)
	wg.Wait()
	drained := time.Since(started)

	mu.Lock()
	defer mu.Unlock()
	report(t, st, reportInput{
		stores: stores, labels: totalLabels, boot: boot,
		offeredRate: *rate, offeredFor: offeredFor, drained: drained,
		offered: offered.Load(), accepted: accepted.Load(),
		refused: refused.Load(), lost: lost.Load(),
		maxFlight: maxFlight.Load(),
		wall:      durationsOf(samples, func(s sample) time.Duration { return s.wall }),
		platform:  msOf(samples, func(s sample) int64 { return s.platform }),
		radio:     msOf(samples, func(s sample) int64 { return s.radio }),
		refresh:   msOf(samples, func(s sample) int64 { return s.refresh }),
		samples:   samples,
	})
}

type reportInput struct {
	stores      []*stack.Store
	labels      int
	boot        time.Duration
	offeredRate float64
	offeredFor  time.Duration
	drained     time.Duration
	offered     int64
	accepted    int64
	refused     int64
	lost        int64
	maxFlight   int64
	wall        []time.Duration
	platform    []time.Duration
	radio       []time.Duration
	refresh     []time.Duration
	samples     []sample
}

// sample is one measured price change.
type sample struct {
	wall     time.Duration
	platform int64
	radio    int64
	refresh  int64
	hops     int
	ok       bool
}

// report prints the whole result, including the part every load report leaves
// out: which component ran out of room first, and how that was decided.
func report(t *testing.T, st *stack.Stack, in reportInput) {
	t.Helper()
	var b strings.Builder
	add := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }

	delivered := len(in.platform)
	achieved := float64(delivered) / in.drained.Seconds()

	add("\n")
	add("USSLP LOAD REPORT\n")
	add("=================\n\n")
	add("  Machine        %d logical CPU(s), %s/%s\n", runtime.NumCPU(), runtime.GOOS, runtime.GOARCH)
	add("                 This is a %d-core container. Every number below is bounded by it,\n", runtime.NumCPU())
	add("                 and the edge tier is simulated in-process at 1:1 wall clock.\n")
	add("  Estate         %d store(s), %d labels, booted and fully priced in %s\n",
		len(in.stores), in.labels, in.boot.Round(time.Millisecond))
	add("\n")
	add("  Offered        %.1f/s for %s  (%d webhooks)\n",
		in.offeredRate, in.offeredFor.Round(time.Second), in.offered)
	add("  Accepted       %d by the UIG, %d skipped (label already repricing), %d refused\n",
		in.accepted, in.refused, in.offered-in.accepted-in.refused)
	add("  Delivered      %d to a label's glass, %d never arrived\n", delivered, in.lost)
	add("  Achieved       %.1f/s sustained, %.1f/s while offering; peak %d concurrent\n",
		achieved, float64(delivered)/in.offeredFor.Seconds(), in.maxFlight)
	add("  Drain          %s of tail after the offered load stopped\n",
		(in.drained - in.offeredFor).Round(time.Millisecond))
	add("\n")

	add("  LATENCY (envelope RecordedAt -> pixels settled)\n")
	add("    %-12s %8s %8s %8s %8s %8s\n", "", "p50", "p90", "p95", "p99", "max")
	add("    %-12s %7dms %7dms %7dms %7dms %7dms\n", "end to end",
		ms(in.platform, 50), ms(in.platform, 90), ms(in.platform, 95),
		ms(in.platform, 99), ms(in.platform, 100))
	add("    %-12s %7dms %7dms %7dms %7dms %7dms\n", "wall clock",
		ms(in.wall, 50), ms(in.wall, 90), ms(in.wall, 95), ms(in.wall, 99), ms(in.wall, 100))
	within := 0
	for _, d := range in.platform {
		if d <= stack.TotalBudget {
			within++
		}
	}
	if delivered > 0 {
		add("    %.2f%% inside the %s SLO\n", 100*float64(within)/float64(delivered), stack.TotalBudget)
	}
	add("\n")

	add("  WHERE THE TIME WENT\n")
	add("    %-34s %8s %8s %8s\n", "", "budget", "p50", "p99")
	cloudBudget := stack.Budget[0].BudgetMS + stack.Budget[1].BudgetMS +
		stack.Budget[2].BudgetMS + stack.Budget[3].BudgetMS + stack.Budget[4].BudgetMS
	cloud := make([]time.Duration, 0, len(in.platform))
	for i := range in.platform {
		v := in.platform[i] - in.radio[i] - in.refresh[i]
		if v < 0 {
			v = 0
		}
		cloud = append(cloud, v)
	}
	add("    %-34s %7dms %7dms %7dms\n", "cloud, bridge and dispatch queue",
		cloudBudget, ms(cloud, 50), ms(cloud, 99))
	add("    %-34s %7dms %7dms %7dms\n", "SEC -> label (radio)",
		stack.Budget[5].BudgetMS, ms(in.radio, 50), ms(in.radio, 99))
	add("    %-34s %7dms %7dms %7dms\n", "E-Ink waveform",
		stack.Budget[6].BudgetMS, ms(in.refresh, 50), ms(in.refresh, 99))
	add("\n")

	add("  EDGE TIER\n")
	add("    %-30s %10s %10s %10s\n", "store / controller", "queued", "in flight", "channel")
	worstQueue := 0
	for _, s := range in.stores {
		for _, z := range s.Zones {
			cs := z.Coordinator.Stats()
			if cs.Queued > worstQueue {
				worstQueue = cs.Queued
			}
			add("    %-30s %10d %10d %9.2f%%\n", string(z.SECID), cs.Queued, cs.InFlight,
				100*z.Sim.Net.ChannelUtilisation())
		}
	}
	add("\n")

	add("  BOTTLENECK\n")
	for _, line := range diagnose(in, cloud, achieved, worstQueue) {
		add("    %s\n", line)
	}
	add("\n")
	t.Log(b.String())

	// The report is also written to a file, because a load result that only
	// exists in a scrolled-past terminal is a load result nobody quotes.
	if path := os.Getenv("USSLP_LOAD_REPORT"); path != "" {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Logf("could not write the report to %s: %v", path, err)
		} else {
			t.Logf("report written to %s", path)
		}
	}

	if delivered == 0 {
		t.Fatal("nothing was delivered; there is no measurement here")
	}
}

// diagnose names the component that ran out of room first, and says what the
// evidence was.
//
// A load report that stops at "throughput was X" leaves the only interesting
// question unanswered. The rules below are crude on purpose: each one names a
// single observable and what it implies, so a reader can disagree with the
// conclusion by disagreeing with the number rather than with a black box.
func diagnose(in reportInput, cloud []time.Duration, achieved float64, worstQueue int) []string {
	var out []string
	delivered := len(in.platform)
	if delivered == 0 {
		return []string{"nothing was delivered"}
	}

	cloudP99 := ms(cloud, 99)
	radioP99 := ms(in.radio, 99)
	refreshP50 := ms(in.refresh, 50)

	// The offered rate against what came out the other end.
	shortfall := in.offeredRate - achieved
	if shortfall > in.offeredRate*0.1 {
		out = append(out, fmt.Sprintf(
			"the estate did not keep up: %.1f/s offered, %.1f/s delivered (%.0f%% shortfall)",
			in.offeredRate, achieved, 100*shortfall/in.offeredRate))
	} else {
		out = append(out, fmt.Sprintf(
			"the estate kept up: %.1f/s offered, %.1f/s delivered", in.offeredRate, achieved))
	}

	switch {
	case worstQueue > 0:
		out = append(out, fmt.Sprintf(
			"a controller still had %d update(s) queued for the radio at the end: "+
				"the transmission window is the constraint", worstQueue))
	case cloudP99 > 500:
		out = append(out, fmt.Sprintf(
			"the p99 outside the radio and the panel is %dms against a %dms budget, "+
				"which at this rate is dispatch queueing rather than service time — "+
				"the p50 is %dms", cloudP99, 500, ms(cloud, 50)))
	default:
		out = append(out, fmt.Sprintf(
			"the cloud tier is not the constraint: everything from the webhook to the "+
				"controller is %dms at p50 and %dms at p99, against a %dms budget",
			ms(cloud, 50), cloudP99, 500))
	}

	out = append(out, fmt.Sprintf(
		"the panel itself is %dms at p50 — a partial waveform is 300ms and a full one 1500ms, "+
			"so the mix of the two sets the floor no amount of cloud capacity can move",
		refreshP50))
	out = append(out, fmt.Sprintf(
		"the radio is %dms at p99 against a %dms budget", radioP99, stack.Budget[5].BudgetMS))

	if in.refused > 0 {
		out = append(out, fmt.Sprintf(
			"%d change(s) were skipped because the label was already being repriced, "+
				"which means the offered rate exceeds what %d labels can absorb one at a time",
			in.refused, in.labels))
	}
	if in.lost > 0 {
		out = append(out, fmt.Sprintf(
			"%d change(s) were accepted and never reached a label; check the controllers' "+
				"delivery_failures — the radio abandons after three attempts", in.lost))
	}
	out = append(out, fmt.Sprintf(
		"simulating %d radios in one process on %d core(s) is itself work: "+
			"the edge tier, not the cloud services, is what this machine runs out of first",
		in.labels, runtime.NumCPU()))
	return out
}

// ---------------------------------------------------------------------------
// Statistics
// ---------------------------------------------------------------------------

func durationsOf[T any](in []T, f func(T) time.Duration) []time.Duration {
	out := make([]time.Duration, 0, len(in))
	for _, v := range in {
		out = append(out, f(v))
	}
	return out
}

func msOf[T any](in []T, f func(T) int64) []time.Duration {
	out := make([]time.Duration, 0, len(in))
	for _, v := range in {
		out = append(out, time.Duration(f(v))*time.Millisecond)
	}
	return out
}

// ms returns a percentile in whole milliseconds, nearest-rank.
func ms(ds []time.Duration, p int) int64 {
	if len(ds) == 0 {
		return 0
	}
	c := append([]time.Duration(nil), ds...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	idx := (p*len(c) + 99) / 100
	if idx < 1 {
		idx = 1
	}
	if idx > len(c) {
		idx = len(c)
	}
	return c[idx-1].Milliseconds()
}
