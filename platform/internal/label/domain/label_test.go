package domain

import (
	"errors"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

var (
	testNow   = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	testLabel = canon.LabelID("lbl-0001")
	testSKU   = canon.SKU("sku-milk-2l")
)

func usd(amount int64) canon.Money { return canon.NewMoney(amount, "USD") }

// newActiveLabel builds a provisioned, assigned label already displaying a
// price, which is the state every price-change invariant is decided against.
func newActiveLabel(t *testing.T) *Label {
	t.Helper()
	l := New(testLabel)
	events, err := l.Provision(Provision{
		TenantID: "acme", StoreID: "store-01", Region: "us-east-1", SECID: "sec-07",
		Currency: "USD", Now: testNow.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := l.Replay(events...); err != nil {
		t.Fatalf("replay provisioning: %v", err)
	}
	events, err = l.Assign(Assign{SKU: testSKU, Now: testNow.Add(-23 * time.Hour)})
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := l.Replay(events...); err != nil {
		t.Fatalf("replay assignment: %v", err)
	}
	events, err = l.ApplyPriceChange(PriceChange{
		SKU: testSKU, Price: usd(249), EffectiveAt: testNow.Add(-22 * time.Hour),
		OccurredAt: testNow.Add(-22 * time.Hour), Now: testNow.Add(-22 * time.Hour),
	}, DefaultPolicy())
	if err != nil {
		t.Fatalf("initial price: %v", err)
	}
	if err := l.Replay(events...); err != nil {
		t.Fatalf("replay initial price: %v", err)
	}
	if l.State != StateActive || l.Price.Amount != 249 || l.Sequence != 1 {
		t.Fatalf("unexpected initial state: %+v", l)
	}
	return l
}

func TestApplyPriceChangeInvariants(t *testing.T) {
	hour := time.Hour
	tests := []struct {
		name string
		// mutate adjusts the label before the command is decided.
		mutate func(*Label)
		// policy overrides the default policy.
		policy *Policy
		cmd    PriceChange
		// wantReason is the expected rejection reason, empty for acceptance.
		wantReason string
		// wantErr is a sentinel the returned error must match.
		wantErr error
		// wantEvent is the event type the command must produce, empty for none.
		wantEvent string
	}{
		{
			name: "accepts a normal price change",
			cmd: PriceChange{
				SKU: testSKU, Price: usd(279), EffectiveAt: testNow,
				OccurredAt: testNow, Now: testNow,
			},
			wantEvent: canon.EvtPriceUpdated,
		},
		{
			name: "rejects a price for a different SKU",
			cmd: PriceChange{
				SKU: "sku-bread", Price: usd(279), EffectiveAt: testNow,
				OccurredAt: testNow, Now: testNow,
			},
			wantReason: ReasonSKUMismatch,
			wantErr:    ErrRejected,
			wantEvent:  canon.EvtPriceRejected,
		},
		{
			name: "rejects a price in another currency",
			cmd: PriceChange{
				SKU: testSKU, Price: canon.NewMoney(279, "EUR"), EffectiveAt: testNow,
				OccurredAt: testNow, Now: testNow,
			},
			wantReason: ReasonCurrencyMismatch,
			wantErr:    ErrRejected,
			wantEvent:  canon.EvtPriceRejected,
		},
		{
			name: "rejects a was-price in another currency",
			cmd: PriceChange{
				SKU: testSKU, Price: usd(279), WasPrice: ptr(canon.NewMoney(300, "GBP")),
				EffectiveAt: testNow, OccurredAt: testNow, Now: testNow,
			},
			wantReason: ReasonCurrencyMismatch,
			wantErr:    ErrRejected,
			wantEvent:  canon.EvtPriceRejected,
		},
		{
			name: "rejects a structurally invalid currency",
			cmd: PriceChange{
				SKU: testSKU, Price: canon.NewMoney(279, "DOLLARS"), EffectiveAt: testNow,
				OccurredAt: testNow, Now: testNow,
			},
			wantReason: ReasonInvalidPrice,
			wantErr:    ErrRejected,
			wantEvent:  canon.EvtPriceRejected,
		},
		{
			name: "rejects a negative price",
			cmd: PriceChange{
				SKU: testSKU, Price: usd(-100), EffectiveAt: testNow,
				OccurredAt: testNow, Now: testNow,
			},
			wantReason: ReasonInvalidPrice,
			wantErr:    ErrRejected,
			wantEvent:  canon.EvtPriceRejected,
		},
		{
			name: "rejects an effective_at older than the grace window",
			cmd: PriceChange{
				SKU: testSKU, Price: usd(279), EffectiveAt: testNow.Add(-7 * 24 * hour),
				OccurredAt: testNow, Now: testNow,
			},
			wantReason: ReasonEffectiveInPast,
			wantErr:    ErrRejected,
			wantEvent:  canon.EvtPriceRejected,
		},
		{
			name: "accepts an effective_at inside the grace window",
			cmd: PriceChange{
				SKU: testSKU, Price: usd(279), EffectiveAt: testNow.Add(-90 * time.Minute),
				OccurredAt: testNow, Now: testNow,
			},
			wantEvent: canon.EvtPriceUpdated,
		},
		{
			name:   "honours a tenant grace override",
			policy: &Policy{EffectiveGrace: time.Minute},
			cmd: PriceChange{
				SKU: testSKU, Price: usd(279), EffectiveAt: testNow.Add(-90 * time.Minute),
				OccurredAt: testNow, Now: testNow,
			},
			wantReason: ReasonEffectiveInPast,
			wantErr:    ErrRejected,
			wantEvent:  canon.EvtPriceRejected,
		},
		{
			name: "rejects an effective_at beyond the scheduling horizon",
			cmd: PriceChange{
				SKU: testSKU, Price: usd(279), EffectiveAt: testNow.Add(365 * 24 * hour),
				OccurredAt: testNow, Now: testNow,
			},
			wantReason: ReasonScheduleTooFar,
			wantErr:    ErrRejected,
			wantEvent:  canon.EvtPriceRejected,
		},
		{
			name: "schedules a future-dated change instead of displaying it",
			cmd: PriceChange{
				SKU: testSKU, Price: usd(279), EffectiveAt: testNow.Add(6 * hour),
				OccurredAt: testNow, Now: testNow, ScheduleID: "sch-1",
			},
			wantEvent: EvtPriceScheduled,
		},
		{
			name: "rejects a fat-fingered price above the guard rail",
			cmd: PriceChange{
				// $2.49 milk priced at $249.00: a hundredfold move.
				SKU: testSKU, Price: usd(24900), EffectiveAt: testNow,
				OccurredAt: testNow, Now: testNow,
			},
			wantReason: ReasonGuardrail,
			wantErr:    ErrRejected,
			wantEvent:  canon.EvtPriceRejected,
		},
		{
			name: "rejects a fat-fingered price below the guard rail",
			cmd: PriceChange{
				SKU: testSKU, Price: usd(2), EffectiveAt: testNow,
				OccurredAt: testNow, Now: testNow,
			},
			wantReason: ReasonGuardrail,
			wantErr:    ErrRejected,
			wantEvent:  canon.EvtPriceRejected,
		},
		{
			name: "accepts an 80% clearance markdown, exactly at the limit",
			cmd: PriceChange{
				SKU: testSKU, Price: usd(50), EffectiveAt: testNow,
				OccurredAt: testNow, Now: testNow, Reason: "clearance",
			},
			wantEvent: canon.EvtPriceUpdated,
		},
		{
			name:   "honours a tenant guard-rail override",
			policy: &Policy{GuardrailFactor: 1.5},
			cmd: PriceChange{
				SKU: testSKU, Price: usd(500), EffectiveAt: testNow,
				OccurredAt: testNow, Now: testNow,
			},
			wantReason: ReasonGuardrail,
			wantErr:    ErrRejected,
			wantEvent:  canon.EvtPriceRejected,
		},
		{
			name: "does not apply the ratio guard below the floor",
			mutate: func(l *Label) {
				l.Price = usd(5)
			},
			cmd: PriceChange{
				SKU: testSKU, Price: usd(50), EffectiveAt: testNow,
				OccurredAt: testNow, Now: testNow,
			},
			wantEvent: canon.EvtPriceUpdated,
		},
		{
			name: "rejects a stale explicit sequence",
			cmd: PriceChange{
				SKU: testSKU, Price: usd(279), Sequence: 1, EffectiveAt: testNow,
				OccurredAt: testNow, Now: testNow,
			},
			wantErr: ErrStaleUpdate,
		},
		{
			name: "accepts an advancing explicit sequence",
			cmd: PriceChange{
				SKU: testSKU, Price: usd(279), Sequence: 9, EffectiveAt: testNow,
				OccurredAt: testNow, Now: testNow,
			},
			wantEvent: canon.EvtPriceUpdated,
		},
		{
			name: "rejects an update older than the displayed price",
			cmd: PriceChange{
				SKU: testSKU, Price: usd(279), EffectiveAt: testNow,
				OccurredAt: testNow.Add(-48 * hour), Now: testNow,
			},
			wantErr: ErrOutOfOrder,
		},
		{
			name: "treats an identical re-application as a no-op",
			cmd: PriceChange{
				SKU: testSKU, Price: usd(249), EffectiveAt: testNow.Add(-22 * hour),
				OccurredAt: testNow, Now: testNow,
			},
			wantErr: ErrStaleUpdate,
		},
		{
			name:   "rejects a price for an unassigned label",
			mutate: func(l *Label) { l.SKU = "" },
			cmd: PriceChange{
				SKU: testSKU, Price: usd(279), EffectiveAt: testNow,
				OccurredAt: testNow, Now: testNow,
			},
			wantReason: ReasonNotAssigned,
			wantErr:    ErrRejected,
			wantEvent:  canon.EvtPriceRejected,
		},
		{
			name:   "rejects a price for a retired label",
			mutate: func(l *Label) { l.State = StateRetired },
			cmd: PriceChange{
				SKU: testSKU, Price: usd(279), EffectiveAt: testNow,
				OccurredAt: testNow, Now: testNow,
			},
			wantReason: ReasonNotAssigned,
			wantErr:    ErrRejected,
			wantEvent:  canon.EvtPriceRejected,
		},
		{
			name:   "still prices an offline label",
			mutate: func(l *Label) { l.State = StateOffline },
			cmd: PriceChange{
				SKU: testSKU, Price: usd(279), EffectiveAt: testNow,
				OccurredAt: testNow, Now: testNow,
			},
			wantEvent: canon.EvtPriceUpdated,
		},
		{
			name: "refuses a command with no decision instant",
			cmd: PriceChange{
				SKU: testSKU, Price: usd(279), EffectiveAt: testNow, OccurredAt: testNow,
			},
			wantErr: ErrInvalidCommand,
		},
		{
			name: "refuses a command with no effective time",
			cmd:  PriceChange{SKU: testSKU, Price: usd(279), Now: testNow},
			// EffectiveAt is required before anything else can be decided.
			wantErr: ErrInvalidCommand,
		},
		{
			name:    "refuses a command with no SKU",
			cmd:     PriceChange{Price: usd(279), EffectiveAt: testNow, Now: testNow},
			wantErr: ErrInvalidCommand,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newActiveLabel(t)
			if tc.mutate != nil {
				tc.mutate(l)
			}
			policy := DefaultPolicy()
			if tc.policy != nil {
				policy = tc.policy.WithDefaults()
			}
			events, err := l.ApplyPriceChange(tc.cmd, policy)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := RejectionReason(err); got != tc.wantReason {
				t.Fatalf("rejection reason = %q, want %q", got, tc.wantReason)
			}
			if tc.wantEvent == "" {
				if len(events) != 0 {
					t.Fatalf("expected no events, got %d (%s)", len(events), events[0].EventType())
				}
				return
			}
			if len(events) == 0 {
				t.Fatalf("expected a %s event, got none", tc.wantEvent)
			}
			last := events[len(events)-1]
			if last.EventType() != tc.wantEvent {
				t.Fatalf("event type = %q, want %q", last.EventType(), tc.wantEvent)
			}
		})
	}
}

func TestApplyPriceChangeAllocatesMonotonicSequences(t *testing.T) {
	l := newActiveLabel(t)
	prices := []int64{279, 299, 259}
	for i, p := range prices {
		at := testNow.Add(time.Duration(i) * time.Minute)
		events, err := l.ApplyPriceChange(PriceChange{
			SKU: testSKU, Price: usd(p), EffectiveAt: at, OccurredAt: at, Now: at,
		}, DefaultPolicy())
		if err != nil {
			t.Fatalf("apply %d: %v", p, err)
		}
		if err := l.Replay(events...); err != nil {
			t.Fatalf("replay: %v", err)
		}
		if want := int64(i + 2); l.Sequence != want {
			t.Fatalf("sequence after %d changes = %d, want %d", i+1, l.Sequence, want)
		}
	}
	if l.Price.Amount != 259 {
		t.Fatalf("final price = %d, want 259", l.Price.Amount)
	}
	if l.PreviousPrice == nil || l.PreviousPrice.Amount != 299 {
		t.Fatalf("previous price = %v, want 299", l.PreviousPrice)
	}
}

func TestScheduledChangeDoesNotDisplayUntilActivated(t *testing.T) {
	l := newActiveLabel(t)
	effective := testNow.Add(3 * time.Hour)
	events, err := l.ApplyPriceChange(PriceChange{
		SKU: testSKU, Price: usd(199), EffectiveAt: effective,
		OccurredAt: testNow, Now: testNow, ScheduleID: "sch-morning",
		PromotionID: "promo-42",
	}, DefaultPolicy())
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if err := l.Replay(events...); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if l.Price.Amount != 249 {
		t.Fatalf("scheduling changed the displayed price to %d", l.Price.Amount)
	}
	if l.Sequence != 1 {
		t.Fatalf("scheduling consumed sequence %d; sequences must be allocated at activation", l.Sequence)
	}
	if len(l.Scheduled) != 1 || l.Scheduled[0].ScheduleID != "sch-morning" {
		t.Fatalf("schedule not recorded: %+v", l.Scheduled)
	}

	if due := l.DueSchedules(testNow.Add(time.Hour)); len(due) != 0 {
		t.Fatalf("schedule reported due an hour early")
	}
	if due := l.DueSchedules(effective); len(due) != 1 {
		t.Fatalf("schedule not due at its effective time")
	}

	events, err = l.ActivateSchedule("sch-morning", DefaultPolicy(), effective)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if err := l.Replay(events...); err != nil {
		t.Fatalf("replay activation: %v", err)
	}
	if l.Price.Amount != 199 || l.Sequence != 2 {
		t.Fatalf("after activation price=%d sequence=%d, want 199/2", l.Price.Amount, l.Sequence)
	}
	if len(l.Scheduled) != 0 {
		t.Fatalf("activated schedule still pending: %+v", l.Scheduled)
	}
}

func TestSchedulingSupersedesAnEarlierScheduleForTheSameSKU(t *testing.T) {
	l := newActiveLabel(t)
	effective := testNow.Add(4 * time.Hour)
	first, err := l.ApplyPriceChange(PriceChange{
		SKU: testSKU, Price: usd(199), EffectiveAt: effective,
		OccurredAt: testNow, Now: testNow, ScheduleID: "sch-a",
	}, DefaultPolicy())
	if err != nil {
		t.Fatalf("first schedule: %v", err)
	}
	if err := l.Replay(first...); err != nil {
		t.Fatalf("replay: %v", err)
	}
	second, err := l.ApplyPriceChange(PriceChange{
		SKU: testSKU, Price: usd(189), EffectiveAt: effective,
		OccurredAt: testNow.Add(time.Minute), Now: testNow.Add(time.Minute), ScheduleID: "sch-b",
	}, DefaultPolicy())
	if err != nil {
		t.Fatalf("second schedule: %v", err)
	}
	if len(second) != 2 || second[0].EventType() != EvtScheduleCancelled {
		t.Fatalf("expected a cancellation then a schedule, got %d events", len(second))
	}
	if err := l.Replay(second...); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(l.Scheduled) != 1 || l.Scheduled[0].Price.Amount != 189 {
		t.Fatalf("superseded schedule survived: %+v", l.Scheduled)
	}
}

func TestActivateExpiredScheduleCancelsInsteadOfDisplaying(t *testing.T) {
	l := newActiveLabel(t)
	effective := testNow.Add(time.Hour)
	expires := testNow.Add(2 * time.Hour)
	events, err := l.ApplyPriceChange(PriceChange{
		SKU: testSKU, Price: usd(199), EffectiveAt: effective, ExpiresAt: &expires,
		OccurredAt: testNow, Now: testNow, ScheduleID: "sch-late",
	}, DefaultPolicy())
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if err := l.Replay(events...); err != nil {
		t.Fatalf("replay: %v", err)
	}
	// The runner reaches it three hours after the promotion window closed.
	events, err = l.ActivateSchedule("sch-late", DefaultPolicy(), testNow.Add(5*time.Hour))
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if len(events) != 1 || events[0].EventType() != EvtScheduleCancelled {
		t.Fatalf("expected a cancellation, got %+v", events)
	}
	if err := l.Replay(events...); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if l.Price.Amount != 249 {
		t.Fatalf("an expired promotional price reached the glass: %d", l.Price.Amount)
	}
}

func TestReassignmentInvalidatesTheDisplayedPrice(t *testing.T) {
	l := newActiveLabel(t)
	events, err := l.Assign(Assign{SKU: "sku-bread", SECID: "sec-09", Now: testNow})
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := l.Replay(events...); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if l.State != StateAssigned {
		t.Fatalf("state after reassignment = %s, want assigned", l.State)
	}
	if l.SECID != "sec-09" {
		t.Fatalf("controller after reassignment = %s", l.SECID)
	}
	// The guard rail must not compare the new product's price against the old
	// product's: bread at $4.99 against milk at $2.49 is a legitimate 2x move,
	// but a $40 line would have been refused had the old price been retained.
	events, err = l.ApplyPriceChange(PriceChange{
		SKU: "sku-bread", Price: usd(4000), EffectiveAt: testNow,
		OccurredAt: testNow, Now: testNow,
	}, DefaultPolicy())
	if err != nil {
		t.Fatalf("pricing the new product: %v", err)
	}
	if events[0].EventType() != canon.EvtPriceUpdated {
		t.Fatalf("expected the new product's price to apply, got %s", events[0].EventType())
	}
}

func TestConfirmDeliveryClosesTheLoop(t *testing.T) {
	l := newActiveLabel(t)
	if l.Pending == nil || l.Pending.Sequence != 1 {
		t.Fatalf("expected a pending update after the initial price")
	}
	events, err := l.ConfirmDelivery(Confirm{
		SECID: "sec-07", Sequence: 1, DeliveredAt: testNow,
		LatencyMS: 1450, MeshHops: 2, RefreshMS: 300, Partial: true,
	})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := l.Replay(events...); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if l.Pending != nil {
		t.Fatalf("pending update survived its confirmation")
	}
	if l.LastDelivery == nil || l.LastDelivery.LatencyMS != 1450 {
		t.Fatalf("delivery not recorded: %+v", l.LastDelivery)
	}

	// A duplicate acknowledgement is routine on an at-least-once mesh and must
	// not overwrite the record with an older one.
	if _, err := l.ConfirmDelivery(Confirm{Sequence: 1, DeliveredAt: testNow}); !errors.Is(err, ErrStaleUpdate) {
		t.Fatalf("duplicate ACK error = %v, want ErrStaleUpdate", err)
	}
}

func TestOfflineLabelReturnsOnConfirmation(t *testing.T) {
	l := newActiveLabel(t)
	events, err := l.MarkOffline("missed_heartbeats", testNow)
	if err != nil {
		t.Fatalf("offline: %v", err)
	}
	if err := l.Replay(events...); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if l.State != StateOffline {
		t.Fatalf("state = %s, want offline", l.State)
	}
	if again, err := l.MarkOffline("missed_heartbeats", testNow); err != nil || len(again) != 0 {
		t.Fatalf("marking an offline label offline should be a no-op, got %d events / %v", len(again), err)
	}
	events, err = l.ConfirmDelivery(Confirm{Sequence: 5, DeliveredAt: testNow, LatencyMS: 900})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := l.Replay(events...); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if l.State != StateActive {
		t.Fatalf("a confirmation should prove reachability; state = %s", l.State)
	}
}

func TestReplayReconstructsIdenticalState(t *testing.T) {
	l := newActiveLabel(t)
	var history []Event
	record := func(events []Event, err error) {
		t.Helper()
		if err != nil && !errors.Is(err, ErrRejected) {
			t.Fatalf("command: %v", err)
		}
		if rerr := l.Replay(events...); rerr != nil {
			t.Fatalf("replay: %v", rerr)
		}
		history = append(history, events...)
	}
	record(l.ApplyPriceChange(PriceChange{
		SKU: testSKU, Price: usd(279), EffectiveAt: testNow, OccurredAt: testNow, Now: testNow,
	}, DefaultPolicy()))
	record(l.ConfirmDelivery(Confirm{Sequence: 2, DeliveredAt: testNow, LatencyMS: 1200}))
	record(l.ApplyPriceChange(PriceChange{
		SKU: testSKU, Price: usd(99999), EffectiveAt: testNow.Add(time.Minute),
		OccurredAt: testNow.Add(time.Minute), Now: testNow.Add(time.Minute),
	}, DefaultPolicy()))
	record(l.MarkOffline("parent_lost", testNow.Add(2*time.Minute)))

	// Rebuild from the same events, including the provisioning history.
	rebuilt := New(testLabel)
	seed, err := rebuilt.Provision(Provision{
		TenantID: "acme", StoreID: "store-01", Region: "us-east-1", SECID: "sec-07",
		Currency: "USD", Now: testNow.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	all := append([]Event{}, seed...)
	all = append(all, LabelAssigned{LabelID: testLabel, SKU: testSKU, OccurredAt: testNow.Add(-23 * time.Hour)})
	all = append(all, PriceApplied{
		LabelID: testLabel, StoreID: "store-01", SECID: "sec-07", SKU: testSKU,
		Price: usd(249), EffectiveAt: testNow.Add(-22 * time.Hour), Sequence: 1,
		Render:     DecideRender(RenderInput{Price: usd(249)}, DefaultPolicy()),
		OccurredAt: testNow.Add(-22 * time.Hour),
	})
	all = append(all, history...)
	if err := rebuilt.Replay(all...); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if rebuilt.Price != l.Price || rebuilt.Sequence != l.Sequence ||
		rebuilt.State != l.State || rebuilt.RejectedCount != l.RejectedCount {
		t.Fatalf("replayed state diverged:\n got %+v\nwant %+v", rebuilt, l)
	}
}

func TestEventRoundTripsThroughStorage(t *testing.T) {
	l := newActiveLabel(t)
	events, err := l.ApplyPriceChange(PriceChange{
		SKU: testSKU, Price: usd(279), WasPrice: ptr(usd(349)),
		PromotionID: "promo-9", EffectiveAt: testNow, OccurredAt: testNow, Now: testNow,
	}, DefaultPolicy())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, want := range events {
		body, err := EncodeEvent(want)
		if err != nil {
			t.Fatalf("encode %s: %v", want.EventType(), err)
		}
		got, err := DecodeEvent(want.EventType(), body)
		if err != nil {
			t.Fatalf("decode %s: %v", want.EventType(), err)
		}
		applied, ok := deref(got).(PriceApplied)
		if !ok {
			t.Fatalf("decoded %s as %T", want.EventType(), got)
		}
		original := want.(PriceApplied)
		if applied.Price != original.Price || applied.Sequence != original.Sequence ||
			applied.Render.Template != original.Render.Template {
			t.Fatalf("round trip lost data:\n got %+v\nwant %+v", applied, original)
		}
	}
	if _, err := DecodeEvent("label.price.invented", []byte(`{}`)); !errors.Is(err, ErrUnknownEvent) {
		t.Fatalf("decoding an unknown event type must fail loudly, got %v", err)
	}
}

func TestPolicySetResolvesPerTenantOverrides(t *testing.T) {
	set := NewPolicySet()
	set.Set("jeweller", Policy{GuardrailFactor: 50})
	if got := set.For("jeweller").GuardrailFactor; got != 50 {
		t.Fatalf("tenant override = %v, want 50", got)
	}
	if got := set.For("grocer").GuardrailFactor; got != DefaultGuardrailFactor {
		t.Fatalf("default factor = %v, want %v", got, DefaultGuardrailFactor)
	}
	// An override that names one knob inherits the rest.
	if got := set.For("jeweller").EffectiveGrace; got != DefaultEffectiveGrace {
		t.Fatalf("inherited grace = %v, want %v", got, DefaultEffectiveGrace)
	}
	var nilSet *PolicySet
	if got := nilSet.For("anyone").GuardrailFactor; got != DefaultGuardrailFactor {
		t.Fatalf("nil policy set must yield defaults, got %v", got)
	}
}

func ptr[T any](v T) *T { return &v }

func TestBasePriceTracksTheEverydayPrice(t *testing.T) {
	l := newActiveLabel(t)
	if l.BasePrice.Amount != 249 {
		t.Fatalf("base price after an ordinary price change = %d, want 249", l.BasePrice.Amount)
	}

	apply := func(price int64, promo canon.PromotionID, at time.Time) {
		t.Helper()
		events, err := l.ApplyPriceChange(PriceChange{
			SKU: testSKU, Price: usd(price), PromotionID: promo,
			EffectiveAt: at, OccurredAt: at, Now: at,
		}, DefaultPolicy())
		if err != nil {
			t.Fatalf("apply %d: %v", price, err)
		}
		if err := l.Replay(events...); err != nil {
			t.Fatalf("replay: %v", err)
		}
	}

	// A promotion leaves the everyday price alone.
	apply(199, "promo-1", testNow)
	if l.Price.Amount != 199 || l.BasePrice.Amount != 249 {
		t.Fatalf("under promotion price=%d base=%d, want 199/249", l.Price.Amount, l.BasePrice.Amount)
	}

	// A second promotion must not turn the first promotion's price into the
	// base — that is what would leave a shelf discounted forever.
	apply(189, "promo-2", testNow.Add(time.Minute))
	if l.BasePrice.Amount != 249 {
		t.Fatalf("a second promotion moved the base price to %d", l.BasePrice.Amount)
	}

	// An ordinary price change re-establishes it.
	apply(279, "", testNow.Add(2*time.Minute))
	if l.BasePrice.Amount != 279 {
		t.Fatalf("base price after a new everyday price = %d, want 279", l.BasePrice.Amount)
	}
}

func TestReassignmentClearsBasePriceAndAttributes(t *testing.T) {
	l := newActiveLabel(t)
	events, err := l.ApplyPriceChange(PriceChange{
		SKU: testSKU, Price: usd(279), EffectiveAt: testNow, OccurredAt: testNow, Now: testNow,
		Attributes: map[string]string{"category": "dairy", "brand": "own-label"},
	}, DefaultPolicy())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := l.Replay(events...); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if l.Category != "dairy" || l.Brand != "own-label" {
		t.Fatalf("merchandising attributes not learned: %q/%q", l.Category, l.Brand)
	}

	events, err = l.Assign(Assign{SKU: "sku-bread", Now: testNow.Add(time.Minute)})
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := l.Replay(events...); err != nil {
		t.Fatalf("replay: %v", err)
	}
	// A different product has a different everyday price and a different
	// category. Keeping either would let a dairy promotion catch a bakery line
	// and would let an expiry revert the shelf to a price for something that is
	// no longer on it.
	if l.BasePrice.Amount != 0 || l.Category != "" || l.Brand != "" {
		t.Fatalf("reassignment kept base=%d category=%q brand=%q", l.BasePrice.Amount, l.Category, l.Brand)
	}
}

func TestIdenticalPriceIsANoOpWhateverItsEffectiveTime(t *testing.T) {
	l := newActiveLabel(t)
	// A re-activation of a promotion already on the glass carries a fresh
	// effective time but changes no pixel, so it must not burn a sequence and a
	// 1.5-second refresh across the store.
	events, err := l.ApplyPriceChange(PriceChange{
		SKU: testSKU, Price: usd(199), PromotionID: "promo-1",
		EffectiveAt: testNow, OccurredAt: testNow, Now: testNow,
	}, DefaultPolicy())
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := l.Replay(events...); err != nil {
		t.Fatalf("replay: %v", err)
	}
	seq := l.Sequence

	later := testNow.Add(time.Hour)
	_, err = l.ApplyPriceChange(PriceChange{
		SKU: testSKU, Price: usd(199), PromotionID: "promo-1",
		EffectiveAt: later, OccurredAt: later, Now: later,
	}, DefaultPolicy())
	if !errors.Is(err, ErrStaleUpdate) {
		t.Fatalf("re-applying an identical price at a later instant = %v, want ErrStaleUpdate", err)
	}
	if l.Sequence != seq {
		t.Fatalf("a no-op consumed a sequence")
	}

	// The same price with a *different* promotion marker is a real change: the
	// badge and the claim differ even though the digits do not.
	events, err = l.ApplyPriceChange(PriceChange{
		SKU: testSKU, Price: usd(199), PromotionID: "promo-2",
		EffectiveAt: later, OccurredAt: later, Now: later,
	}, DefaultPolicy())
	if err != nil {
		t.Fatalf("changing the promotion behind an identical price: %v", err)
	}
	if len(events) != 1 || events[0].EventType() != canon.EvtPriceUpdated {
		t.Fatalf("expected a price event, got %+v", events)
	}

	// A future-dated command is always scheduled, never dismissed as a no-op:
	// it is a request to change the shelf later, and something may change it in
	// between.
	future := later.Add(3 * time.Hour)
	events, err = l.ApplyPriceChange(PriceChange{
		SKU: testSKU, Price: usd(199), PromotionID: "promo-2",
		EffectiveAt: future, OccurredAt: later, Now: later, ScheduleID: "sch-1",
	}, DefaultPolicy())
	if err != nil {
		t.Fatalf("future-dated identical price: %v", err)
	}
	if len(events) != 1 || events[0].EventType() != EvtPriceScheduled {
		t.Fatalf("a future-dated change must schedule, got %+v", events)
	}
}
