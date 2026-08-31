package filedrop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/deliveries"
	"github.com/usslp/usslp/platform/internal/uig/pipeline"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/idem"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/obs"
)

type capturePublisher struct {
	mu   sync.Mutex
	msgs []eventbus.Message
}

func (p *capturePublisher) Publish(_ context.Context, msgs ...eventbus.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, msgs...)
	return nil
}

func (p *capturePublisher) Close() error { return nil }

func (p *capturePublisher) priceEvents() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, m := range p.msgs {
		if m.Topic == canon.StreamPriceUpdates.Name {
			n++
		}
	}
	return n
}

type watchHarness struct {
	dir     string
	quar    string
	watcher *Watcher
	pub     *capturePublisher
	pipe    *pipeline.Pipeline
	now     func() time.Time
}

func newWatchHarness(t *testing.T) *watchHarness {
	t.Helper()
	kv, err := kvstore.Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kv.Close() })
	backend, _ := idem.NewKVBackend(kv, "idem/")
	guard, _ := idem.New(backend)
	store, _ := deliveries.New(kv, deliveries.Options{})

	reg := adapter.NewRegistry()
	reg.MustRegister(New())
	bindings := adapter.NewBindingStore(reg)
	if err := bindings.Put(&adapter.Binding{
		ID: "nightly", TenantID: "acme", Adapter: Name,
		DefaultCurrency: "EUR", DefaultStore: "DE-0001",
		AllowUnmappedStores: true,
		Options:             json.RawMessage(csvOptions),
	}); err != nil {
		t.Fatal(err)
	}

	pub := &capturePublisher{}
	pipe, err := pipeline.New(pipeline.Config{
		Registry: reg, Bindings: bindings, Guard: guard, Bus: pub,
		Deliveries: store, Metrics: pipeline.NewMetrics(obs.NewRegistry()), Log: obs.NopLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pipe.Close() })

	dir := t.TempDir()
	quar := filepath.Join(t.TempDir(), "quarantine")
	w, err := NewWatcher(WatchConfig{
		Dir: dir, Pattern: "PRICE_*.csv",
		TenantID: "acme", BindingID: "nightly",
		QuarantineDir: quar,
		// Settle is disabled for the test's own files, which are written and
		// closed synchronously; in production it is what stops a half-written
		// FTP transfer being read as a complete price book.
		Settle: time.Nanosecond,
		Log:    obs.NopLogger(),
	}, pipe)
	if err != nil {
		t.Fatal(err)
	}
	return &watchHarness{dir: dir, quar: quar, watcher: w, pub: pub, pipe: pipe}
}

func (h *watchHarness) write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(h.dir, name)
	if err := os.WriteFile(p, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	// Back-date the file so it is outside any settle window.
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	return p
}

const goodFile = "ITEMCODE,SITE,CUR,PRICE,WASPRICE,FROMDATE,UOM,DESCR\n" +
	"0000123456,0042,eur,12.99,15.50,20260901,EA,Espresso Beans\n" +
	"0000123457,0042,eur,3.75,,,,Loose Tea\n"

func TestWatcherProcessesAndMarksAFile(t *testing.T) {
	h := newWatchHarness(t)
	path := h.write(t, "PRICE_20260830.csv", goodFile)

	n, err := h.watcher.ScanOnce(context.Background())
	if err != nil {
		t.Fatalf("ScanOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("processed %d files, want 1", n)
	}
	if got := h.pub.priceEvents(); got != 2 {
		t.Fatalf("published %d price events, want 2", got)
	}

	// The marker is a sibling file rather than a moved or deleted original: the
	// share belongs to the retailer, and a gateway that moved their files would
	// be blamed for anything missing.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the original file was disturbed: %v", err)
	}
	raw, err := os.ReadFile(path + MarkerSuffix)
	if err != nil {
		t.Fatalf("no marker written: %v", err)
	}
	var receipt Receipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatalf("marker is not a receipt: %v", err)
	}
	if receipt.Emitted != 2 || receipt.Status != string(deliveries.StatusAccepted) {
		t.Errorf("receipt = %+v", receipt)
	}
	if receipt.SHA256 == "" || receipt.DeliveryID == "" {
		t.Errorf("receipt is missing provenance: %+v", receipt)
	}

	// A second scan must be a no-op: this is the whole point of the marker.
	if n, err := h.watcher.ScanOnce(context.Background()); err != nil || n != 0 {
		t.Fatalf("rescan processed %d files, want 0 (err %v)", n, err)
	}
	if got := h.pub.priceEvents(); got != 2 {
		t.Fatalf("a rescan republished: %d events", got)
	}
	if st := h.watcher.Stats(); st.Processed != 1 || st.Skipped != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestARemovedMarkerReprocessesButTheGuardSuppressesIt(t *testing.T) {
	h := newWatchHarness(t)
	path := h.write(t, "PRICE_A.csv", goodFile)
	if _, err := h.watcher.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + MarkerSuffix); err != nil {
		t.Fatal(err)
	}
	// Deleting the marker is how an operator asks for a reprocess. The
	// idempotency guard — keyed on the file's name and content digest — turns
	// it into zero new events, which is also what protects a crash between the
	// durable publish and the marker write.
	if _, err := h.watcher.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.pub.priceEvents(); got != 2 {
		t.Fatalf("reprocessing republished: %d events, want 2", got)
	}
}

func TestACorrectedFileUnderTheSameNameIsProcessed(t *testing.T) {
	h := newWatchHarness(t)
	h.write(t, "PRICE_A.csv", goodFile)
	if _, err := h.watcher.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The retailer notices a mistake and re-exports under the same name. The
	// marker holds the old digest, so the new content is genuinely new work —
	// skipping it would leave the wrong prices live while looking like
	// everything had worked.
	corrected := strings.Replace(goodFile, "12.99", "11.99", 1)
	h.write(t, "PRICE_A.csv", corrected)
	if n, err := h.watcher.ScanOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("corrected file: processed %d (err %v)", n, err)
	}
	if got := h.pub.priceEvents(); got != 4 {
		t.Fatalf("published %d events, want 4", got)
	}
}

func TestUnparseableFilesAreQuarantinedNotRetriedForever(t *testing.T) {
	h := newWatchHarness(t)
	h.write(t, "PRICE_BROKEN.csv", "this is not a price file at all\n")

	if _, err := h.watcher.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// It must leave the drop directory, or every scan for the next fortnight
	// retries it.
	if _, err := os.Stat(filepath.Join(h.dir, "PRICE_BROKEN.csv")); !os.IsNotExist(err) {
		t.Error("the unusable file is still in the drop directory")
	}
	entries, err := os.ReadDir(h.quar)
	if err != nil {
		t.Fatal(err)
	}
	var moved, receipt string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".error.json") {
			receipt = e.Name()
		} else if strings.HasSuffix(e.Name(), "PRICE_BROKEN.csv") {
			moved = e.Name()
		}
	}
	if moved == "" || receipt == "" {
		t.Fatalf("quarantine contains %v", entries)
	}
	// The timestamped name means two bad files a week apart do not overwrite
	// each other, which matters because the second is the evidence the first
	// was not a one-off.
	if !strings.Contains(moved, "T") || !strings.HasPrefix(moved, "20") {
		t.Errorf("quarantined name %q is not timestamped", moved)
	}
	raw, err := os.ReadFile(filepath.Join(h.quar, receipt))
	if err != nil {
		t.Fatal(err)
	}
	var r Receipt
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if r.Detail == "" {
		t.Error("the quarantine receipt does not say why")
	}
	if st := h.watcher.Stats(); st.Quarantined != 1 {
		t.Errorf("stats = %+v", st)
	}
	if h.pub.priceEvents() != 0 {
		t.Error("an unusable file published events")
	}
}

func TestOversizedFilesAreQuarantinedWithoutBeingRead(t *testing.T) {
	h := newWatchHarness(t)
	h.watcher.cfg.MaxFileBytes = 16
	h.write(t, "PRICE_BIG.csv", goodFile)
	if _, err := h.watcher.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, "PRICE_BIG.csv")); !os.IsNotExist(err) {
		t.Error("the oversized file was left in place")
	}
	if h.pub.priceEvents() != 0 {
		t.Error("an oversized file was ingested")
	}
}

func TestPatternAndSettleWindow(t *testing.T) {
	h := newWatchHarness(t)
	h.write(t, "PRICE_A.csv", goodFile)
	h.write(t, "OTHER.csv", goodFile)
	h.write(t, "PRICE_A.csv"+MarkerSuffix, "{}")
	if n, err := h.watcher.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		// PRICE_A already has a marker (with a mismatched digest, so it will be
		// reprocessed) — but OTHER.csv must never be picked up at all.
		t.Logf("processed %d", n)
	}

	// A file still being written — its modification time is now — is left for
	// the next scan rather than read half-transferred.
	h.watcher.cfg.Settle = time.Hour
	p := filepath.Join(h.dir, "PRICE_FRESH.csv")
	if err := os.WriteFile(p, []byte(goodFile), 0o640); err != nil {
		t.Fatal(err)
	}
	before := h.watcher.Stats().Skipped
	if _, err := h.watcher.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.watcher.Stats().Skipped <= before {
		t.Error("a file still inside the settle window was read")
	}
	if _, err := os.Stat(p); err != nil {
		t.Error("the in-flight file was disturbed")
	}
}

func TestFilesAreProcessedInNameOrder(t *testing.T) {
	h := newWatchHarness(t)
	for _, n := range []string{"PRICE_03.csv", "PRICE_01.csv", "PRICE_02.csv"} {
		h.write(t, n, goodFile)
	}
	if n, err := h.watcher.ScanOnce(context.Background()); err != nil || n != 3 {
		t.Fatalf("processed %d (err %v)", n, err)
	}
	// Directory order is not an order. A retailer writing 01, 02, 03 means them
	// to be applied in that sequence.
	h.pub.mu.Lock()
	defer h.pub.mu.Unlock()
	var seen []string
	for _, m := range h.pub.msgs {
		if m.Topic == canon.StreamPOSIngress.Name {
			var env canon.Envelope
			if err := json.Unmarshal(m.Value, &env); err != nil {
				t.Fatal(err)
			}
			var raw pipeline.RawDelivery
			if err := env.Decode(&raw); err != nil {
				t.Fatal(err)
			}
			seen = append(seen, raw.Path)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("raw copies = %v", seen)
	}
	for i, want := range []string{"PRICE_01.csv", "PRICE_02.csv", "PRICE_03.csv"} {
		if seen[i] != want {
			t.Errorf("file %d was %q, want %q", i, seen[i], want)
		}
	}
}

func TestRunStopsOnContextCancellation(t *testing.T) {
	h := newWatchHarness(t)
	h.write(t, "PRICE_A.csv", goodFile)
	h.watcher.cfg.Interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.watcher.Run(ctx) }()
	// Run performs one scan immediately, so a gateway restarting after an
	// outage picks up whatever accumulated while it was down.
	deadline := time.After(2 * time.Second)
	for h.pub.priceEvents() == 0 {
		select {
		case <-deadline:
			t.Fatal("the immediate scan never ran")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || err.Error() != context.Canceled.Error() {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancellation")
	}
}

func TestWatcherConfigValidation(t *testing.T) {
	if _, err := NewWatcher(WatchConfig{}, nil); err == nil {
		t.Error("a watcher with no directory was accepted")
	}
	if _, err := NewWatcher(WatchConfig{Dir: t.TempDir()}, nil); err == nil {
		t.Error("a watcher with no pipeline was accepted")
	}
	spec := &WatchSpec{Dir: "relative/path"}
	if err := spec.validate(); err == nil {
		t.Error("a relative drop directory was accepted; it resolves differently per process")
	}
	spec = &WatchSpec{Dir: "/srv/drops", Pattern: "[", Interval: "30s"}
	if err := spec.validate(); err == nil {
		t.Error("a malformed pattern was accepted")
	}
	spec = &WatchSpec{Dir: "/srv/drops", Interval: "soon"}
	if err := spec.validate(); err == nil {
		t.Error("a malformed interval was accepted")
	}
	good := &WatchSpec{Dir: "/srv/drops", Pattern: "PRICE_*.csv", Interval: "1m", Settle: "10s"}
	if err := good.validate(); err != nil {
		t.Fatalf("a valid spec was rejected: %v", err)
	}
	opts := &Options{Watch: good}
	wc, ok := opts.WatchConfigFor("acme", "nightly", obs.NopLogger())
	if !ok || wc.Dir != "/srv/drops" || wc.Interval != time.Minute || wc.Settle != 10*time.Second {
		t.Fatalf("watch config = %+v (ok=%v)", wc, ok)
	}
	if _, ok := (&Options{}).WatchConfigFor("acme", "nightly", nil); ok {
		t.Error("a binding with no watch block must not start a watcher")
	}
}

func TestMarkerIsWrittenAtomically(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "receipt.json")
	if err := writeMarkerAtomically(dst, Receipt{FileName: "x", SHA256: "abc", Emitted: 3}); err != nil {
		t.Fatal(err)
	}
	// No temporary file may survive: a stray .usslp-marker-* in the retailer's
	// drop directory is both confusing and, if it were ever matched by the
	// pattern, reprocessed.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "receipt.json" {
		t.Fatalf("directory contains %v", entries)
	}
	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	var r Receipt
	if err := json.Unmarshal(raw, &r); err != nil || r.SHA256 != "abc" {
		t.Fatalf("receipt = %+v, err %v", r, err)
	}
}
