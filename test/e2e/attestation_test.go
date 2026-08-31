package e2e

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// TestTamperedPriceIsRefused is the security claim in INTERFACE-CONTRACTS §5,
// attacked at exactly the point the contract says is defended.
//
// The threat model is explicit: "a compromised controller, a corrupted mesh
// frame, or an attacker with write access to the store's broker cannot change a
// displayed price; they can only prevent one from changing." So the test takes
// a genuine, correctly signed price update off the store's own MQTT broker,
// changes the price in it, and republishes it on the same topic — which is
// precisely what an attacker with write access to that broker would do.
//
// The claim is that the shelf does not move. Three things have to hold: the
// controller refuses the update, the previous price stays on the glass, and a
// compliance alert is raised.
func TestTamperedPriceIsRefused(t *testing.T) {
	st := newStack(t, smallStore(1, 6))
	store := st.Stores()[0]
	tap := priceTapFor(t, st, store)
	tg := pick(t, st, 0, 0, 0)

	// A real price change first, so there is a genuine attested envelope to
	// tamper with and a known-good price on the glass to defend.
	honest := tg.nudge(101)
	pushPrice(t, st, tg, honest)
	if ok, why := st.GlassMatches(tg.Zone, tg.Label, honest); !ok {
		t.Fatalf("the honest price never landed, so there is nothing to defend: %s", why)
	}

	captured, ok := tap.await(tg.Label, 0, 5*time.Second)
	if !ok {
		t.Fatal("no attested price update was captured from the store's broker")
	}
	beforeAlerts := len(tg.Zone.Controller.ComplianceAlerts())
	beforeStats := tg.Zone.Controller.Stats()

	// The attack: same envelope, same signature, one field changed. The price
	// is halved and the sequence advanced so the update is not simply
	// discarded as stale — which would prove nothing about the attestation.
	var env canon.Envelope
	if err := json.Unmarshal(captured.Raw, &env); err != nil {
		t.Fatalf("decoding the captured envelope: %v", err)
	}
	tampered := captured.Update
	tampered.Price = canon.NewMoney(honest.Amount/2, honest.Currency)
	tampered.Sequence = captured.Update.Sequence + 1
	forged, err := env.WithPayload(tampered)
	if err != nil {
		t.Fatalf("re-encoding the tampered update: %v", err)
	}
	body, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	tap.publish(t, captured.Topic, body)

	// The controller must refuse it. Waiting for the refusal rather than
	// sleeping for a fixed period, and then waiting a further beat to be sure
	// no delivery follows.
	eventually(t, 10*time.Second, "the controller to refuse the tampered price", func() bool {
		return tg.Zone.Controller.Stats().AttestationFailed > beforeStats.AttestationFailed
	})
	time.Sleep(2 * time.Second)

	// 1. The previous price is still on the glass.
	if ok, why := st.GlassMatches(tg.Zone, tg.Label, honest); !ok {
		t.Errorf("the tampered price changed what the shelf shows: %s", why)
	}
	rec, _ := tg.Zone.Controller.Record(tg.Label)
	if rec.Price.Cmp(tampered.Price) == 0 {
		t.Fatalf("the controller accepted a price it could not verify: %s", rec.Price.Display())
	}
	if rec.Sequence != captured.Update.Sequence {
		t.Errorf("the refused update advanced the controller's sequence from %d to %d; "+
			"an update that cannot be verified must leave no trace at all",
			captured.Update.Sequence, rec.Sequence)
	}

	// 2. A compliance alert was raised, naming the label, the price that was
	//    held and the key the forgery claimed.
	alerts := tg.Zone.Controller.ComplianceAlerts()
	if len(alerts) <= beforeAlerts {
		t.Fatal("no compliance alert was raised for a refused price")
	}
	alert := alerts[len(alerts)-1]
	if alert.LabelID != tg.Label {
		t.Errorf("the alert names %s, not %s", alert.LabelID, tg.Label)
	}
	if alert.HeldPrice.Cmp(honest) != 0 {
		t.Errorf("the alert says the label held %s; it was showing %s",
			alert.HeldPrice.Display(), honest.Display())
	}
	t.Logf("refused: %s (key %s); %s kept showing %s",
		alert.Reason, alert.KeyID, alert.LabelID, alert.HeldPrice.Display())

	// 3. And the platform can still change the price legitimately afterwards:
	//    an attack must not wedge the label.
	after := tg.nudge(17)
	pushPrice(t, st, tg, after)
	if ok, why := st.GlassMatches(tg.Zone, tg.Label, after); !ok {
		t.Errorf("a legitimate price after the attack did not land: %s", why)
	}
}

// TestUnattestedPriceIsRefused is the same defence against a simpler attack: an
// update with no signature at all, which is what a naive injection looks like.
func TestUnattestedPriceIsRefused(t *testing.T) {
	st := newStack(t, smallStore(1, 6))
	store := st.Stores()[0]
	tap := priceTapFor(t, st, store)
	tg := pick(t, st, 0, 0, 1)

	honest := tg.nudge(59)
	pushPrice(t, st, tg, honest)
	captured, ok := tap.await(tg.Label, 0, 5*time.Second)
	if !ok {
		t.Fatal("no attested price update was captured")
	}
	before := tg.Zone.Controller.Stats().AttestationFailed

	var env canon.Envelope
	if err := json.Unmarshal(captured.Raw, &env); err != nil {
		t.Fatal(err)
	}
	naked := captured.Update
	naked.Price = canon.NewMoney(1, honest.Currency)
	naked.Sequence = captured.Update.Sequence + 1
	naked.Attestation = canon.Attestation{}
	forged, err := env.WithPayload(naked)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(forged)
	tap.publish(t, captured.Topic, body)

	eventually(t, 10*time.Second, "the controller to refuse an unsigned price", func() bool {
		return tg.Zone.Controller.Stats().AttestationFailed > before
	})
	if ok, why := st.GlassMatches(tg.Zone, tg.Label, honest); !ok {
		t.Errorf("an unsigned price moved the shelf: %s", why)
	}
}
