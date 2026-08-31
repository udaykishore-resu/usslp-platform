package app_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	labeladapters "github.com/usslp/usslp/platform/internal/label/adapters"
	labelapp "github.com/usslp/usslp/platform/internal/label/app"
	labelports "github.com/usslp/usslp/platform/internal/label/ports"
	"github.com/usslp/usslp/platform/internal/registry/adapters"
	"github.com/usslp/usslp/platform/internal/registry/domain"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/eventbus"
	"github.com/usslp/usslp/platform/pkg/eventlog"
	"github.com/usslp/usslp/platform/pkg/eventstore"
	"github.com/usslp/usslp/platform/pkg/kvstore"
	"github.com/usslp/usslp/platform/pkg/pki"
)

// TestControllerEnrolmentIsAFleetEventAndNotALabelEvent is the regression test
// for a cross-service defect the README used to carry as a known gap: the
// Device Registry announced every
// device kind as `device.label.provisioned`, and the Label Service's directory
// projection — which consumes `device-events` and has no reason to care about
// controllers — decoded each one as a label, refused it for having no SECID,
// retried it five times and dead-lettered it. One dead-letter per controller
// and an error-log burst on every boot.
//
// The fix is a contract and not a filter, so this test asserts both halves of
// it against the real thing: a real registry provisioning real certified
// hardware, publishing through the real bus adapter onto a real event log with
// real consumer groups and a real dead-letter stream.
//
//  1. the Label Service's own projection sees the controller's enrolment and
//     skips it — no handler error, and nothing on `dead-letter`;
//  2. a consumer that *does* want controller events still receives it, and can
//     tell it is a controller from the message headers alone, without
//     deserialising the payload;
//  3. a label enrolled through the same path still reaches the directory, so
//     the first two are not being bought by dropping everything.
func TestControllerEnrolmentIsAFleetEventAndNotALabelEvent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	bus, err := eventlog.Open("", eventlog.WithSync(eventlog.SyncNever))
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	// canon's partition counts are sized for the estate; four is enough for the
	// assignment path to be real and cheap enough for a unit test, exactly as
	// usslpd clamps them for a single process.
	if err := bus.EnsureStreams(ctx,
		clamp(canon.StreamDeviceEvents, 4), clamp(canon.StreamDLQ, 4)); err != nil {
		t.Fatalf("ensure streams: %v", err)
	}

	at := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	profile := pki.TestProfile()
	hierarchy, err := pki.Bootstrap(pki.BootstrapConfig{Profile: &profile, Now: at})
	if err != nil {
		t.Fatalf("bootstrap pki hierarchy: %v", err)
	}
	h := newHarnessOn(t, t.TempDir(), at, hierarchy, adapters.NewBusPublisher(bus))

	// The Label Service's directory projection, wired to its real adapters.
	directory, repo := labelReadSide(t)
	projection, err := labelapp.NewDirectoryProjection(labelapp.Deps{
		Directory: directory, Repo: repo,
	}, labelapp.FixedCurrency("USD"))
	if err != nil {
		t.Fatalf("build the label directory projection: %v", err)
	}

	failures := &handlerFailures{}
	runConsumer(t, bus, "label-service.devices", func(ctx context.Context, m eventbus.Message) error {
		if err := projection.HandleMessage(ctx, m); err != nil {
			failures.add(err)
			return err
		}
		return nil
	})

	// A consumer with the OTA service's interest: it wants controllers, and it
	// routes on the header rather than on the body.
	fleet := &headerLog{}
	runConsumer(t, bus, "ota-service.devices", func(_ context.Context, m eventbus.Message) error {
		fleet.add(m.Headers[eventbus.HeaderEventType], m.Key)
		return nil
	})

	dlq := &headerLog{}
	runConsumerOn(t, bus, canon.StreamDLQ.Name, "dlq-watch", func(_ context.Context, m eventbus.Message) error {
		dlq.add(m.Headers[eventbus.HeaderDLQOrigin], m.Key)
		return nil
	})

	// Enrol a controller and, under it, a label. Both go through the real
	// zero-touch path: chain verified, identity read out of the certificate,
	// manufacturing record compared.
	sec := h.manufacture("sec-01", domain.KindSEC, 0x11)
	label := h.manufacture("lbl-0001", domain.KindLabel, 0x12)
	h.ingest(sec, label)
	h.provision(sec, "", "")
	h.provision(label, canon.SECID("sec-01"), "aisle-01")

	// (3) first, because it is the one that has to become true rather than stay
	// true, and reaching it means both consumers have caught up past the
	// controller's record.
	waitFor(t, 10*time.Second, "the label to reach the fan-out directory", func() bool {
		p, err := directory.Lookup(ctx, canon.LabelID("lbl-0001"))
		return err == nil && p.SECID == canon.SECID("sec-01")
	})

	// (2) the controller's enrolment is on the stream, under its own name. This
	// is reported rather than fatal so that a regression shows the whole shape
	// of the defect — the missing fleet event *and* the dead-letters — in one
	// run instead of one symptom at a time.
	if !awaitTrue(2*time.Second, func() bool { return fleet.has(canon.EvtSECProvisioned, "sec-01") }) {
		t.Error("no consumer of device-events saw device.sec.provisioned for the controller")
	}
	if fleet.has(canon.EvtLabelProvisioned, "sec-01") {
		t.Error("the controller was announced as device.label.provisioned; a consumer " +
			"routing on the usslp-event-type header cannot tell the tiers apart")
	}
	if !fleet.has(canon.EvtLabelProvisioned, "lbl-0001") {
		t.Error("the label was not announced as device.label.provisioned")
	}

	// (1) the Label Service neither failed nor poisoned anything. The grace
	// period is generous against the retry ladder: five attempts at a 1 ms base
	// with exponential backoff is tens of milliseconds, so a dead-letter that is
	// going to happen has happened long before this returns.
	time.Sleep(500 * time.Millisecond)
	if errs := failures.all(); len(errs) != 0 {
		t.Errorf("the label directory failed %d device-events record(s); the first was: %v",
			len(errs), errs[0])
	}
	if n := dlq.len(); n != 0 {
		t.Errorf("%d record(s) were dead-lettered; a device the Label Service has no "+
			"use for must be skipped, not declared poison", n)
	}
}

// TestProvisioningEventNameCarriesTheDeviceKind pins the producer half of the
// contract: one event name per tier, and a payload whose kind agrees with it.
//
// It is a separate test from the cross-service one because this is the part
// other consumers are written against, and it should fail with a name a
// reviewer can act on when someone collapses the three names back into one.
func TestProvisioningEventNameCarriesTheDeviceKind(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	cases := []struct {
		device    string
		kind      domain.DeviceKind
		eui       uint64
		parent    canon.SECID
		eventType string
	}{
		{"lbl-0001", domain.KindLabel, 0x21, canon.SECID("sec-01"), canon.EvtLabelProvisioned},
		{"sec-01", domain.KindSEC, 0x22, "", canon.EvtSECProvisioned},
		{"sgu-01", domain.KindSGU, 0x23, "", canon.EvtSGUProvisioned},
	}
	devices := make([]enrolled, 0, len(cases))
	for _, c := range cases {
		devices = append(devices, h.manufacture(c.device, c.kind, c.eui))
	}
	h.ingest(devices...)
	for i, c := range cases {
		h.provision(devices[i], c.parent, "")

		envs := h.pub.ofType(canon.StreamDeviceEvents.Name, c.eventType)
		var found *canon.Envelope
		for i := range envs {
			if envs[i].AggregateID == c.device {
				found = &envs[i]
			}
		}
		if found == nil {
			t.Fatalf("%s (%s) was not announced as %s", c.device, c.kind, c.eventType)
		}
		var payload canon.DeviceProvisioned
		if err := found.Decode(&payload); err != nil {
			t.Fatalf("decode %s payload: %v", c.eventType, err)
		}
		if payload.Kind != string(c.kind) {
			t.Errorf("%s payload kind = %q, want %q", c.eventType, payload.Kind, c.kind)
		}
		if got := canon.ProvisionedEventFor(payload.Kind); got != c.eventType {
			t.Errorf("the payload's kind maps to %s, but the envelope says %s", got, c.eventType)
		}
	}

	// Nothing but a label may travel under the label's name.
	for _, env := range h.pub.ofType(canon.StreamDeviceEvents.Name, canon.EvtLabelProvisioned) {
		var payload canon.DeviceProvisioned
		if err := env.Decode(&payload); err != nil {
			t.Fatalf("decode device.label.provisioned payload: %v", err)
		}
		if payload.Kind != canon.DeviceKindLabel {
			t.Errorf("a %s was announced as device.label.provisioned (%s)",
				payload.Kind, env.AggregateID)
		}
	}
}

// TestLabelDirectorySkipsAControllerUnderTheOldEventName covers the records the
// fix cannot reach by name: a `device-events` history written before the event
// names were split, which a directory rebuild replays from offset 0.
//
// Both shapes appear there — a payload that states its kind, and the pre-split
// payload that states nothing and is identified by having no parent controller,
// which is the very field the projection used to reject it for.
func TestLabelDirectorySkipsAControllerUnderTheOldEventName(t *testing.T) {
	t.Parallel()
	directory, repo := labelReadSide(t)
	projection, err := labelapp.NewDirectoryProjection(labelapp.Deps{
		Directory: directory, Repo: repo,
	}, labelapp.FixedCurrency("USD"))
	if err != nil {
		t.Fatalf("build the label directory projection: %v", err)
	}

	cases := []struct {
		name    string
		device  string
		payload canon.DeviceProvisioned
	}{
		{
			name:   "kind stated",
			device: "sec-01",
			payload: canon.DeviceProvisioned{
				LabelID: canon.LabelID("sec-01"), Kind: canon.DeviceKindSEC,
				Serial: "SEC-0001", HardwareTier: "sec-v2",
			},
		},
		{
			name:   "pre-split payload, no kind at all",
			device: "sec-02",
			payload: canon.DeviceProvisioned{
				LabelID: canon.LabelID("sec-02"),
				Serial:  "SEC-0002", HardwareTier: "sec-v2",
			},
		},
		{
			name:   "a gateway, pre-split",
			device: "sgu-01",
			payload: canon.DeviceProvisioned{
				LabelID: canon.LabelID("sgu-01"),
				Serial:  "SGU-0001", HardwareTier: "sgu-v1",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := c.payload
			p.StoreID = canon.StoreID("store-0042")
			p.ProvisionedAt = time.Now().UTC()
			// No SECID: a controller and a gateway are their own radio
			// authority and are never parented to another controller.
			env, err := canon.NewEnvelope(canon.EvtLabelProvisioned, domain.AggregateDevice,
				c.device, canon.TenantID("acme"), p)
			if err != nil {
				t.Fatalf("build envelope: %v", err)
			}
			env.StoreID = canon.StoreID("store-0042")
			env.Source = "device-registry"

			if err := projection.HandleEnvelope(t.Context(), env); err != nil {
				t.Fatalf("the projection refused %s announced under the old name: %v", c.device, err)
			}
			if _, err := directory.Lookup(t.Context(), canon.LabelID(c.device)); !errors.Is(err, labelports.ErrNotFound) {
				t.Errorf("%s was written into the label directory; Lookup = %v, want ErrNotFound",
					c.device, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

// clamp returns a stream with its partition count reduced for a single-process
// test. Ordering in USSLP is per partition *key*, so the count is a parallelism
// choice and nothing the assertions here depend on.
func clamp(s canon.Stream, partitions int) canon.Stream {
	if s.Partitions > partitions {
		s.Partitions = partitions
	}
	return s
}

// labelReadSide builds the Label Service's real directory and aggregate
// repository over their own stores.
func labelReadSide(t *testing.T) (labelports.Directory, labelports.Repository) {
	t.Helper()
	kv, err := kvstore.OpenWith(kvstore.Options{Dir: t.TempDir(), Sync: kvstore.SyncNever})
	if err != nil {
		t.Fatalf("open label kvstore: %v", err)
	}
	t.Cleanup(func() { _ = kv.Close() })
	es, err := eventstore.New(kv)
	if err != nil {
		t.Fatalf("open label eventstore: %v", err)
	}
	t.Cleanup(func() { _ = es.Close() })
	dir, err := labeladapters.NewKVDirectory(kv)
	if err != nil {
		t.Fatalf("build the label directory: %v", err)
	}
	repo, err := labeladapters.NewEventStoreRepository(es, labeladapters.RepositoryConfig{})
	if err != nil {
		t.Fatalf("build the label repository: %v", err)
	}
	return dir, repo
}

// runConsumer subscribes a group to `device-events` from the beginning, with a
// retry ladder short enough that a record which is going to be dead-lettered is
// dead-lettered inside the test's grace period.
func runConsumer(t *testing.T, bus *eventlog.Log, group string, h eventbus.Handler) {
	t.Helper()
	runConsumerOn(t, bus, canon.StreamDeviceEvents.Name, group, h)
}

func runConsumerOn(t *testing.T, bus *eventlog.Log, topic, group string, h eventbus.Handler) {
	t.Helper()
	c, err := bus.Subscribe(eventbus.SubscribeOptions{
		Group: group, Topics: []string{topic}, FromBeginning: true,
		MaxRetries: 5, RetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("subscribe %s to %s: %v", group, topic, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx, h) }()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = c.Close()
	})
}

// handlerFailures records every error a consumer's handler returned.
type handlerFailures struct {
	mu   sync.Mutex
	errs []error
}

func (f *handlerFailures) add(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs = append(f.errs, err)
}

func (f *handlerFailures) all() []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]error(nil), f.errs...)
}

// headerLog records what a consumer saw without ever decoding a payload, which
// is the whole point of the assertion it backs.
type headerLog struct {
	mu   sync.Mutex
	seen [][2]string
}

func (l *headerLog) add(a, b string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, [2]string{a, b})
}

func (l *headerLog) has(a, b string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.seen {
		if s[0] == a && s[1] == b {
			return true
		}
	}
	return false
}

func (l *headerLog) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.seen)
}

// waitFor polls until a condition holds, failing the test if it never does.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	if !awaitTrue(within, cond) {
		t.Fatalf("timed out after %s waiting for %s", within, what)
	}
}

// awaitTrue polls until a condition holds or the deadline passes, reporting
// which happened.
func awaitTrue(within time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(within)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}
