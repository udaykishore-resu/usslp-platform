package sap

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
)

const signingKey = "sap-ale-shared-secret"

// pricatIDoc is a recorded-shape PRICAT transmission. It exercises everything
// that makes IDocs hard at once: a legacy code page, two IDOCs in one POST,
// repeated header segments for two plants, repeated item segments, zero-padded
// material numbers, an explicit decimal shift finer than the currency, a
// reference condition for the list price, a trailing SAP sign, and a withdrawal
// action that must not become a price.
const pricatIDoc = `<?xml version="1.0" encoding="ISO-8859-1"?>
<PRICAT_CREATE01>
  <IDOC BEGIN="1">
    <EDI_DC40 SEGMENT="1">
      <TABNAM>EDI_DC40</TABNAM>
      <DOCNUM>0000000000173842</DOCNUM>
      <STATUS>30</STATUS>
      <DIRECT>1</DIRECT>
      <IDOCTYP>PRICAT_CREATE01</IDOCTYP>
      <MESTYP>PRICAT</MESTYP>
      <SNDPRN>SAPPRD100</SNDPRN>
      <RCVPRN>USSLP</RCVPRN>
      <CREDAT>20260830</CREDAT>
      <CRETIM>140211</CRETIM>
    </EDI_DC40>
    <E1PRHDR SEGMENT="1">
      <ACTION>004</ACTION>
      <WERKS>0042</WERKS>
      <VKORG>DE01</VKORG>
      <WAERS>EUR</WAERS>
      <DATAB>20260901</DATAB>
      <DATBI>20260930</DATBI>
      <E1PRITM SEGMENT="1">
        <MATNR>000000000000123456</MATNR>
        <EAN11>4006381333931</EAN11>
        <MAKTX>Mgrsli 500g</MAKTX>
        <KBETR>0000249</KBETR>
        <KPEIN>1</KPEIN>
        <KMEIN>ST</KMEIN>
        <DECSHIFT>2</DECSHIFT>
        <KONWA>EUR</KONWA>
        <E1PRREF SEGMENT="1">
          <KSCHL>VKP0</KSCHL>
          <KBETR>0000299</KBETR>
          <DECSHIFT>2</DECSHIFT>
        </E1PRREF>
      </E1PRITM>
      <E1PRITM SEGMENT="1">
        <MATNR>000000000000654321</MATNR>
        <KBETR>0000075050</KBETR>
        <KPEIN>1</KPEIN>
        <KMEIN>ST</KMEIN>
        <DECSHIFT>4</DECSHIFT>
      </E1PRITM>
    </E1PRHDR>
    <E1PRHDR SEGMENT="1">
      <ACTION>004</ACTION>
      <WERKS>0088</WERKS>
      <WAERS>EUR</WAERS>
      <E1PRITM SEGMENT="1">
        <MATNR>000000000000123456</MATNR>
        <KBETR>0000259-</KBETR>
        <DECSHIFT>2</DECSHIFT>
      </E1PRITM>
    </E1PRHDR>
    <E1PRHDR SEGMENT="1">
      <ACTION>003</ACTION>
      <WERKS>0042</WERKS>
      <WAERS>EUR</WAERS>
      <E1PRITM SEGMENT="1">
        <MATNR>000000000000999999</MATNR>
        <KBETR>0000100</KBETR>
        <DECSHIFT>2</DECSHIFT>
      </E1PRITM>
    </E1PRHDR>
  </IDOC>
  <IDOC BEGIN="1">
    <EDI_DC40 SEGMENT="1">
      <DOCNUM>0000000000173843</DOCNUM>
      <MESTYP>PRICAT</MESTYP>
      <CREDAT>20260830</CREDAT>
      <CRETIM>140212</CRETIM>
    </EDI_DC40>
    <E1PRHDR SEGMENT="1">
      <ACTION>009</ACTION>
      <WERKS>0042</WERKS>
      <WAERS>JPY</WAERS>
      <E1PRITM SEGMENT="1">
        <MATNR>000000000000777000</MATNR>
        <KBETR>0000024900</KBETR>
        <DECSHIFT>2</DECSHIFT>
      </E1PRITM>
    </E1PRHDR>
  </IDOC>
</PRICAT_CREATE01>`

// latin1Body re-encodes the IDoc's one non-ASCII character as the single
// ISO-8859-1 byte a real SAP port would emit.
func latin1Body(t *testing.T) []byte {
	t.Helper()
	// U+00FC (ü) is 0xFC in ISO-8859-1. The fixture holds the ASCII letter 'g'
	// in its place so the Go source stays plain; swapping it here produces a
	// genuinely non-UTF-8 document.
	s := strings.Replace(pricatIDoc, "Mgrsli 500g", "M\xfcrsli 500g", 1)
	return []byte(s)
}

func bindWith(t *testing.T, optionsJSON string) *adapter.Binding {
	t.Helper()
	reg := adapter.NewRegistry()
	reg.MustRegister(New())
	store := adapter.NewBindingStore(reg)
	b := &adapter.Binding{
		ID: "sap-eu", TenantID: "acme", Adapter: Name,
		DefaultCurrency: "EUR",
		Secrets:         adapter.Secrets{HMACKey: signingKey},
	}
	if optionsJSON != "" {
		b.Options = json.RawMessage(optionsJSON)
	}
	if err := store.Put(b); err != nil {
		t.Fatalf("binding: %v", err)
	}
	got, err := store.Get("acme", "sap-eu")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func testDelivery(t *testing.T, body []byte, optionsJSON string) *adapter.Delivery {
	t.Helper()
	b := bindWith(t, optionsJSON)
	d := &adapter.Delivery{
		TenantID: "acme", BindingID: b.ID, Binding: b,
		Method: http.MethodPost, Path: "/v1/ingest/acme/sap-eu",
		ContentType: "application/xml; charset=ISO-8859-1",
		Body:        body,
		ReceivedAt:  time.Date(2026, 8, 30, 14, 3, 0, 0, time.UTC),
		Headers:     http.Header{},
	}
	d.Headers.Set(HeaderSignature,
		adapter.EncodeSignature(adapter.SignHMACSHA256(signingKey, d.Body), adapter.EncodingHex))
	return d
}

func TestIngestPRICATTransmission(t *testing.T) {
	a := New()
	d := testDelivery(t, latin1Body(t), "")
	if err := a.Verify(context.Background(), d); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	changes, err := a.Ingest(context.Background(), d)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	// Two plants in the first IDoc plus one in the second; the withdrawal
	// action produces nothing.
	if len(changes) != 4 {
		t.Fatalf("got %d changes, want 4: %+v", len(changes), changes)
	}

	c := changes[0]
	// SAP pads MATNR to eighteen characters; the retailer's catalogue does not.
	if c.SKU != "123456" {
		t.Errorf("sku = %q, want the zero-stripped 123456", c.SKU)
	}
	if c.StoreID != "0042" {
		t.Errorf("store = %q, want the plant code", c.StoreID)
	}
	// KBETR 0000249 with DECSHIFT 2 is 2.49 EUR, exactly 249 minor units.
	if c.Price.Amount != 249 || c.Price.Currency != "EUR" {
		t.Errorf("price = %+v", c.Price)
	}
	// The reference VKP0 condition is the recommended retail price shown struck
	// through on the label.
	if c.WasPrice == nil || c.WasPrice.Amount != 299 {
		t.Errorf("was price = %+v", c.WasPrice)
	}
	if c.UnitMeasure != "ST" {
		t.Errorf("unit measure = %q", c.UnitMeasure)
	}
	// The code page is why this test exists: without a charset reader the
	// document does not parse at all, and with the wrong one this reads as
	// mojibake on a shelf.
	if got := c.Attributes["description"]; got != "Mürsli 500g" {
		t.Errorf("description = %q, want the ISO-8859-1 text decoded", got)
	}
	if c.Attributes["sap_docnum"] != "0000000000173842" || c.Attributes["sap_action"] != "004" {
		t.Errorf("attributes = %v", c.Attributes)
	}
	if c.Attributes["sap_decshift"] != "2" || c.Attributes["sap_kbetr"] != "0000249" {
		t.Errorf("the original condition value must be retained for audit: %v", c.Attributes)
	}
	if !c.EffectiveAt.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("effective_at = %s", c.EffectiveAt)
	}
	if c.ExpiresAt == nil || !c.ExpiresAt.Equal(time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)) {
		t.Errorf("expires_at = %v", c.ExpiresAt)
	}

	// 0000075050 with DECSHIFT 4 is 7.5050 EUR: finer than the currency, so it
	// rounds half away from zero to 751 cents rather than truncating to 750.
	if changes[1].Price.Amount != 751 {
		t.Errorf("sub-cent condition = %d, want 751", changes[1].Price.Amount)
	}

	// The second plant's segment repeats the same material, and its condition
	// carries SAP's trailing minus sign.
	if changes[2].StoreID != "0088" || changes[2].Price.Amount != -259 {
		t.Errorf("second plant = %+v", changes[2])
	}

	// The second IDoc trades in a zero-decimal currency: 24900 shifted by 2 is
	// 249 yen, not 24,900.
	if changes[3].Price.Currency != "JPY" || changes[3].Price.Amount != 249 {
		t.Errorf("JPY condition = %+v", changes[3].Price)
	}

	if !d.SourceTime.Equal(time.Date(2026, 8, 30, 14, 2, 11, 0, time.UTC)) {
		t.Errorf("source time = %s, want the IDoc's CREDAT/CRETIM", d.SourceTime)
	}
}

func TestIdempotencyIsDocnumActionAndTimestamp(t *testing.T) {
	a := New()
	parts := a.IdempotencyParts(testDelivery(t, latin1Body(t), ""))
	if len(parts) != 3 {
		t.Fatalf("parts = %v", parts)
	}
	if !strings.Contains(parts[0], "0000000000173842") || !strings.Contains(parts[0], "0000000000173843") {
		t.Errorf("docnum part = %q, want both IDoc numbers", parts[0])
	}
	if !strings.HasPrefix(parts[1], "action=") || !strings.Contains(parts[1], "004") {
		t.Errorf("action part = %q", parts[1])
	}
	if !strings.Contains(parts[2], "20260830140211") {
		t.Errorf("timestamp part = %q", parts[2])
	}

	// The identity must survive re-serialisation: a retailer's middleware that
	// reindents the XML between the first send and the ALE resend produces
	// different bytes for the same document, and only the document identity
	// catches that.
	reindented := strings.ReplaceAll(string(latin1Body(t)), "\n  ", "\n      ")
	again := a.IdempotencyParts(testDelivery(t, []byte(reindented), ""))
	if len(again) != 3 || again[0] != parts[0] || again[2] != parts[2] {
		t.Fatalf("re-serialised identity = %v, want %v", again, parts)
	}

	// A different IDoc number is different work.
	other := strings.Replace(string(latin1Body(t)), "0000000000173842", "0000000000173999", 1)
	if a.IdempotencyParts(testDelivery(t, []byte(other), ""))[0] == parts[0] {
		t.Error("two different IDoc numbers produced the same identity")
	}
}

func TestBareIDocWithoutTheTransmissionWrapper(t *testing.T) {
	a := New()
	bare := `<?xml version="1.0" encoding="UTF-8"?>
<IDOC BEGIN="1">
  <EDI_DC40><DOCNUM>0000000000000001</DOCNUM><MESTYP>PRICAT</MESTYP><CREDAT>20260830</CREDAT><CRETIM>100000</CRETIM></EDI_DC40>
  <E1PRHDR><ACTION>009</ACTION><WERKS>0001</WERKS><WAERS>EUR</WAERS>
    <E1PRITM><MATNR>000000000000000042</MATNR><KBETR>0000199</KBETR><DECSHIFT>2</DECSHIFT></E1PRITM>
  </E1PRHDR>
</IDOC>`
	d := testDelivery(t, []byte(bare), "")
	changes, err := a.Ingest(context.Background(), d)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(changes) != 1 || changes[0].SKU != "42" || changes[0].Price.Amount != 199 {
		t.Fatalf("changes = %+v", changes)
	}
	if a.IdempotencyParts(d) == nil {
		t.Error("a bare IDoc must still yield an identity")
	}
}

func TestPricingUnitMustDivideExactly(t *testing.T) {
	a := New()
	// KBETR 1000 with KPEIN 3 is 3.33... per unit: a price nobody authorised.
	body := `<PRICAT01><IDOC><EDI_DC40><DOCNUM>1</DOCNUM><CREDAT>20260830</CREDAT><CRETIM>100000</CRETIM></EDI_DC40>
	<E1PRHDR><ACTION>004</ACTION><WERKS>0001</WERKS><WAERS>EUR</WAERS>
	  <E1PRITM><MATNR>1</MATNR><KBETR>1000</KBETR><KPEIN>3</KPEIN><DECSHIFT>2</DECSHIFT></E1PRITM>
	  <E1PRITM><MATNR>2</MATNR><KBETR>1000</KBETR><KPEIN>4</KPEIN><DECSHIFT>2</DECSHIFT></E1PRITM>
	</E1PRHDR></IDOC></PRICAT01>`
	changes, err := a.Ingest(context.Background(), testDelivery(t, []byte(body), ""))
	if len(changes) != 1 || changes[0].Price.Amount != 250 {
		t.Fatalf("changes = %+v, want only the exactly divisible one", changes)
	}
	partial, ok := adapter.IsPartial(err)
	if !ok || partial.Failures[0].Reason != "price_unit_not_exact" {
		t.Fatalf("err = %v", err)
	}

	// A retailer who genuinely wants rounding opts in explicitly.
	changes2, err := a.Ingest(context.Background(),
		testDelivery(t, []byte(body), `{"allow_inexact_price_unit":true}`))
	if err != nil {
		t.Fatalf("with rounding enabled: %v", err)
	}
	if len(changes2) != 2 || changes2[0].Price.Amount != 333 {
		t.Fatalf("rounded changes = %+v", changes2)
	}
}

func TestActionFilteringAndOptions(t *testing.T) {
	a := New()
	// Deletions are excluded by default, so the withdrawal in the fixture
	// produced nothing. Including them explicitly changes that.
	d := testDelivery(t, latin1Body(t), `{"actions":["003"]}`)
	changes, err := a.Ingest(context.Background(), d)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(changes) != 1 || changes[0].SKU != "999999" {
		t.Fatalf("changes = %+v, want only the withdrawal", changes)
	}

	byEAN := testDelivery(t, latin1Body(t), `{"sku_field":"EAN11"}`)
	changes, err = a.Ingest(context.Background(), byEAN)
	if err == nil {
		t.Fatal("items with no EAN11 should have been reported as row failures")
	}
	if len(changes) != 1 || changes[0].SKU != "4006381333931" {
		t.Fatalf("EAN-keyed changes = %+v", changes)
	}

	noStrip := testDelivery(t, latin1Body(t), `{"strip_material_zeros":false}`)
	changes, _ = a.Ingest(context.Background(), noStrip)
	if changes[0].SKU != "000000000000123456" {
		t.Errorf("sku = %q, want the padded form", changes[0].SKU)
	}

	for _, bad := range []string{`{"sku_field":"MYSTERY"}`, `{"default_decimal_shift":99}`, `{"nope":1}`} {
		if _, err := a.CompileOptions(json.RawMessage(bad)); err == nil {
			t.Errorf("options %s were accepted", bad)
		}
	}
}

func TestMalformedTransmissionsAreQuarantinable(t *testing.T) {
	a := New()
	for name, body := range map[string]string{
		"not xml":           `{"json":"instead"}`,
		"truncated":         `<PRICAT01><IDOC><EDI_DC40><DOCNUM>1`,
		"no idocs":          `<PRICAT01></PRICAT01>`,
		"unknown code page": `<?xml version="1.0" encoding="EBCDIC-CP-BE"?><PRICAT01><IDOC/></PRICAT01>`,
	} {
		_, err := a.Ingest(context.Background(), testDelivery(t, []byte(body), ""))
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		cls := adapter.Classify(err)
		if cls.Kind != adapter.FailureMalformed {
			t.Errorf("%s: kind = %s, want malformed", name, cls.Kind)
		}
		if cls.Kind.HTTPStatus() >= 500 {
			t.Errorf("%s: status %d — an unparseable IDoc must not be retried into a blocked ALE queue",
				name, cls.Kind.HTTPStatus())
		}
	}
}

func TestBadSegmentsAreIsolated(t *testing.T) {
	a := New()
	body := `<PRICAT01><IDOC><EDI_DC40><DOCNUM>1</DOCNUM><CREDAT>20260830</CREDAT><CRETIM>100000</CRETIM></EDI_DC40>
	<E1PRHDR><ACTION>004</ACTION><WERKS>0001</WERKS><WAERS>EUR</WAERS>
	  <E1PRITM><MATNR>1</MATNR><KBETR>0000199</KBETR><DECSHIFT>2</DECSHIFT></E1PRITM>
	  <E1PRITM><MATNR></MATNR><KBETR>0000199</KBETR><DECSHIFT>2</DECSHIFT></E1PRITM>
	  <E1PRITM><MATNR>3</MATNR><DECSHIFT>2</DECSHIFT></E1PRITM>
	  <E1PRITM><MATNR>4</MATNR><KBETR>0000199</KBETR><DECSHIFT>zz</DECSHIFT></E1PRITM>
	  <E1PRITM><MATNR>5</MATNR><KBETR>0000299</KBETR><DECSHIFT>2</DECSHIFT></E1PRITM>
	</E1PRHDR></IDOC></PRICAT01>`
	changes, err := a.Ingest(context.Background(), testDelivery(t, []byte(body), ""))
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want the two usable segments", len(changes))
	}
	partial, ok := adapter.IsPartial(err)
	if !ok || len(partial.Failures) != 3 {
		t.Fatalf("err = %v", err)
	}
	reasons := []string{
		partial.Failures[0].Reason, partial.Failures[1].Reason, partial.Failures[2].Reason,
	}
	want := []string{"missing_material", "missing_condition_value", "bad_decimal_shift"}
	for i := range want {
		if reasons[i] != want[i] {
			t.Errorf("failure %d reason = %q, want %q", i, reasons[i], want[i])
		}
	}
	if partial.Total != 5 {
		t.Errorf("Total = %d, want 5", partial.Total)
	}
}

func TestVerifyAcceptsMTLSPeerInsteadOfASignature(t *testing.T) {
	a := New()
	d := testDelivery(t, latin1Body(t), "")
	d.Binding.Secrets.PeerCommonNames = []string{"sap-prd.acme.example"}
	d.Binding.Secrets.HMACKey = ""
	d.Headers.Del(HeaderSignature)

	d.PeerIdentity = "sap-prd.acme.example"
	if err := a.Verify(context.Background(), d); err != nil {
		t.Fatalf("an allowed peer was rejected: %v", err)
	}
	d.PeerIdentity = "attacker.example"
	if err := a.Verify(context.Background(), d); err == nil {
		t.Fatal("an unlisted peer was accepted")
	}
	d.PeerIdentity = ""
	if err := a.Verify(context.Background(), d); err == nil {
		t.Fatal("a request with no client certificate was accepted")
	}
}

func TestNoEndDateSentinelIsNotAnExpiry(t *testing.T) {
	a := New()
	body := `<PRICAT01><IDOC><EDI_DC40><DOCNUM>1</DOCNUM><CREDAT>20260830</CREDAT><CRETIM>100000</CRETIM></EDI_DC40>
	<E1PRHDR><ACTION>004</ACTION><WERKS>0001</WERKS><WAERS>EUR</WAERS><DATBI>99991231</DATBI>
	  <E1PRITM><MATNR>1</MATNR><KBETR>0000199</KBETR><DECSHIFT>2</DECSHIFT></E1PRITM>
	</E1PRHDR></IDOC></PRICAT01>`
	changes, err := a.Ingest(context.Background(), testDelivery(t, []byte(body), ""))
	if err != nil {
		t.Fatal(err)
	}
	// SAP's 99991231 means "no end date". Scheduling a price to lapse in the
	// year 9999 is harmless but pollutes every downstream projection.
	if changes[0].ExpiresAt != nil {
		t.Errorf("expires_at = %v, want none", changes[0].ExpiresAt)
	}
}
