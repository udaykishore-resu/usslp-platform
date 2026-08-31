package oracle

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
)

const (
	wsUser = "USSLP_RIB"
	wsPass = "rib-shared-password"
)

// itemPriceDescEnvelope is a recorded-shape RIB ItemPriceDesc publish.
const itemPriceDescEnvelope = `<?xml version="1.0" encoding="UTF-8"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/">
  <soapenv:Header>
    <wsse:Security xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd">
      <wsse:UsernameToken>
        <wsse:Username>USSLP_RIB</wsse:Username>
        <wsse:Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordText">rib-shared-password</wsse:Password>
        <wsse:Nonce>bm9uY2U=</wsse:Nonce>
      </wsse:UsernameToken>
    </wsse:Security>
    <rib:RibMessageHeader xmlns:rib="http://www.oracle.com/retail/integration/rib/v1">
      <rib:family>Price</rib:family>
      <rib:messageType>ItemPriceDescCre</rib:messageType>
      <rib:messageId>RIB-0000000091733</rib:messageId>
      <rib:publishTime>2026-08-30T14:02:11Z</rib:publishTime>
    </rib:RibMessageHeader>
  </soapenv:Header>
  <soapenv:Body>
    <PublishItemPriceDescCreate xmlns="http://www.oracle.com/retail/integration/base/bo/ItemPriceDesc/v1">
      <ItemPriceDesc>
        <store>0042</store>
        <currency_code>USD</currency_code>
        <price_change_id>PC-88213</price_change_id>
        <ItemPrice>
          <item>100123456</item>
          <item_desc>Espresso Beans 1kg</item_desc>
          <selling_unit_retail>12.99</selling_unit_retail>
          <selling_uom>EA</selling_uom>
          <standard_unit_retail>15.50</standard_unit_retail>
          <multi_unit_retail>35.00</multi_unit_retail>
          <multi_units>3</multi_units>
          <effective_date>2026-09-01T00:00:00Z</effective_date>
          <end_date>2026-09-30T23:59:59Z</end_date>
          <promotion>PROMO-77</promotion>
        </ItemPrice>
        <ItemPrice>
          <item>100123457</item>
          <selling_unit_retail>3.75</selling_unit_retail>
          <selling_uom>EA</selling_uom>
          <loc>0088</loc>
        </ItemPrice>
      </ItemPriceDesc>
    </PublishItemPriceDescCreate>
  </soapenv:Body>
</soapenv:Envelope>`

func bindWith(t *testing.T, optionsJSON string) *adapter.Binding {
	t.Helper()
	reg := adapter.NewRegistry()
	reg.MustRegister(New())
	store := adapter.NewBindingStore(reg)
	b := &adapter.Binding{
		ID: "oracle-rib", TenantID: "acme", Adapter: Name,
		DefaultCurrency: "USD",
		Secrets:         adapter.Secrets{Username: wsUser, SharedToken: wsPass},
	}
	if optionsJSON != "" {
		b.Options = json.RawMessage(optionsJSON)
	}
	if err := store.Put(b); err != nil {
		t.Fatalf("binding: %v", err)
	}
	got, err := store.Get("acme", "oracle-rib")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func testDelivery(t *testing.T, body, optionsJSON string) *adapter.Delivery {
	t.Helper()
	b := bindWith(t, optionsJSON)
	return &adapter.Delivery{
		TenantID: "acme", BindingID: b.ID, Binding: b,
		Method: http.MethodPost, Path: "/v1/ingest/acme/oracle-rib/soap",
		ContentType: "text/xml; charset=utf-8",
		Body:        []byte(body),
		ReceivedAt:  time.Date(2026, 8, 30, 14, 2, 12, 0, time.UTC),
		Headers:     http.Header{},
	}
}

func TestIngestItemPriceDesc(t *testing.T) {
	a := New()
	d := testDelivery(t, itemPriceDescEnvelope, "")
	if err := a.Verify(context.Background(), d); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	changes, err := a.Ingest(context.Background(), d)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want 2", len(changes))
	}

	c := changes[0]
	if c.SKU != "100123456" || c.StoreID != "0042" {
		t.Errorf("identity = %q @ %q", c.SKU, c.StoreID)
	}
	if c.Price.Amount != 1299 || c.Price.Currency != "USD" {
		t.Errorf("price = %+v", c.Price)
	}
	if c.WasPrice == nil || c.WasPrice.Amount != 1550 {
		t.Errorf("was = %+v", c.WasPrice)
	}
	if c.UnitMeasure != "EA" || c.PromotionID != "PROMO-77" {
		t.Errorf("change = %+v", c)
	}
	// A multi-unit retail is not the shelf price: the price is still per
	// selling unit, and the "3 for 35.00" belongs in the attributes.
	if c.Attributes["oracle_multi_units"] != "3" || c.Attributes["oracle_multi_unit_retail"] != "35.00 USD" {
		t.Errorf("multi-unit attributes = %v", c.Attributes)
	}
	if c.Attributes["rib_message_id"] != "RIB-0000000091733" {
		t.Errorf("the RIB message id was dropped: %v", c.Attributes)
	}
	if !c.EffectiveAt.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) || c.ExpiresAt == nil {
		t.Errorf("dates = %s / %v", c.EffectiveAt, c.ExpiresAt)
	}
	// A per-item <loc> overrides the descriptor's store.
	if changes[1].StoreID != "0088" {
		t.Errorf("second item store = %q, want the per-item loc", changes[1].StoreID)
	}
	if !d.SourceTime.Equal(time.Date(2026, 8, 30, 14, 2, 11, 0, time.UTC)) {
		t.Errorf("source time = %s", d.SourceTime)
	}
}

func TestVerifyUsernameToken(t *testing.T) {
	a := New()
	ctx := context.Background()

	wrongPass := strings.Replace(itemPriceDescEnvelope, wsPass, "guessed", 1)
	if err := a.Verify(ctx, testDelivery(t, wrongPass, "")); err == nil {
		t.Fatal("a wrong password was accepted")
	}
	wrongUser := strings.Replace(itemPriceDescEnvelope, "<wsse:Username>USSLP_RIB</wsse:Username>",
		"<wsse:Username>someone_else</wsse:Username>", 1)
	if err := a.Verify(ctx, testDelivery(t, wrongUser, "")); err == nil {
		t.Fatal("a wrong username was accepted")
	}
	// A digest token is refused rather than silently downgraded: accepting it
	// would require the server to keep the password recoverable.
	digest := strings.Replace(itemPriceDescEnvelope, PasswordTextType,
		"http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest", 1)
	if cls := adapter.Classify(a.Verify(ctx, testDelivery(t, digest, ""))); cls.Reason != "unsupported_password_type" {
		t.Errorf("digest token reason = %q", cls.Reason)
	}
	// An envelope that will not parse is reported as unauthorized, so an
	// unauthenticated caller cannot use the error to distinguish "your XML is
	// bad" from "your password is bad".
	cls := adapter.Classify(a.Verify(ctx, testDelivery(t, `<not xml`, "")))
	if cls.Kind != adapter.FailureUnauthorized {
		t.Errorf("unparseable envelope classified as %s", cls.Kind)
	}
}

func TestVerifyAcceptsMTLSPeer(t *testing.T) {
	a := New()
	d := testDelivery(t, itemPriceDescEnvelope, "")
	d.Binding.Secrets.PeerCommonNames = []string{"rib.acme.example"}
	d.PeerIdentity = "rib.acme.example"
	if err := a.Verify(context.Background(), d); err != nil {
		t.Fatalf("an allowed peer was rejected: %v", err)
	}

	// A binding that requires both must still check the token.
	both := testDelivery(t, strings.Replace(itemPriceDescEnvelope, wsPass, "wrong", 1),
		`{"require_ws_security":true}`)
	both.Binding.Secrets.PeerCommonNames = []string{"rib.acme.example"}
	both.PeerIdentity = "rib.acme.example"
	if err := a.Verify(context.Background(), both); err == nil {
		t.Fatal("require_ws_security did not enforce the token")
	}
}

func TestIdempotencyUsesTheRIBMessageID(t *testing.T) {
	a := New()
	parts := a.IdempotencyParts(testDelivery(t, itemPriceDescEnvelope, ""))
	if len(parts) != 3 || parts[2] != "RIB-0000000091733" {
		t.Fatalf("parts = %v", parts)
	}
	noHeader := strings.Replace(itemPriceDescEnvelope, "RIB-0000000091733", "", 1)
	if got := a.IdempotencyParts(testDelivery(t, noHeader, "")); got != nil {
		t.Errorf("a message with no id must defer to the body digest, got %v", got)
	}
}

func TestZeroRetailPolicy(t *testing.T) {
	a := New()
	body := strings.Replace(itemPriceDescEnvelope,
		"<selling_unit_retail>12.99</selling_unit_retail>",
		"<selling_unit_retail>0.00</selling_unit_retail>", 1)

	// By default a zero retail is refused, because putting "0.00" on a shelf is
	// worse than quarantining a row.
	changes, err := a.Ingest(context.Background(), testDelivery(t, body, ""))
	partial, ok := adapter.IsPartial(err)
	if !ok || partial.Failures[0].Reason != "zero_retail" {
		t.Fatalf("err = %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("the other item should still have been ingested: %+v", changes)
	}

	// An estate where zero genuinely means withdrawal says so explicitly.
	changes, err = a.Ingest(context.Background(), testDelivery(t, body, `{"zero_retail_is_withdrawal":true}`))
	if err != nil {
		t.Fatalf("with the option set: %v", err)
	}
	if len(changes) != 1 || changes[0].SKU != "100123457" {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestMalformedEnvelopesAreClassifiedForQuarantine(t *testing.T) {
	a := New()
	for name, body := range map[string]string{
		"not xml":    `{"json":true}`,
		"not soap":   `<Something><Else/></Something>`,
		"empty body": `<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body/></soapenv:Envelope>`,
		"truncated":  `<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body>`,
	} {
		_, err := a.Ingest(context.Background(), testDelivery(t, body, ""))
		if err == nil {
			t.Errorf("%s: expected an error", name)
			continue
		}
		cls := adapter.Classify(err)
		if cls.Kind != adapter.FailureMalformed {
			t.Errorf("%s: kind = %s", name, cls.Kind)
		}
		if cls.Kind.HTTPStatus() >= 500 {
			t.Errorf("%s: status %d — RIB retries a 5xx forever and blocks every price behind it",
				name, cls.Kind.HTTPStatus())
		}
	}
}

func TestBadItemsAreIsolated(t *testing.T) {
	a := New()
	body := `<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body>
	  <PublishItemPriceDesc><ItemPriceDesc><store>S1</store><currency_code>USD</currency_code>
	    <ItemPrice><item>A</item><selling_unit_retail>1.00</selling_unit_retail></ItemPrice>
	    <ItemPrice><item></item><selling_unit_retail>2.00</selling_unit_retail></ItemPrice>
	    <ItemPrice><item>C</item></ItemPrice>
	    <ItemPrice><item>D</item><selling_unit_retail>not a price</selling_unit_retail></ItemPrice>
	    <ItemPrice><item>E</item><selling_unit_retail>5.00</selling_unit_retail></ItemPrice>
	  </ItemPriceDesc></PublishItemPriceDesc>
	</soapenv:Body></soapenv:Envelope>`
	changes, err := a.Ingest(context.Background(), testDelivery(t, body, ""))
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want the two usable ones", len(changes))
	}
	partial, ok := adapter.IsPartial(err)
	if !ok || len(partial.Failures) != 3 {
		t.Fatalf("err = %v", err)
	}
	want := []string{"missing_item", "missing_retail", "price_unusable"}
	for i, w := range want {
		if partial.Failures[i].Reason != w {
			t.Errorf("failure %d reason = %q, want %q", i, partial.Failures[i].Reason, w)
		}
	}
}

func TestSOAPFaultIsAClientFaultOnA4xx(t *testing.T) {
	// SOAP 1.1 nominally puts faults on 500. RIB's error hospital treats a 5xx
	// as retryable, so a message that will never parse would block the queue
	// behind it forever. The fault therefore travels on the pipeline's own 4xx.
	for status, wantCode := range map[int]FaultCode{
		400: FaultClient,
		401: FaultClient,
		422: FaultClient,
		503: FaultServer,
		500: FaultServer,
	} {
		code, httpStatus := FaultFor(status)
		if code != wantCode {
			t.Errorf("status %d -> %s, want %s", status, code, wantCode)
		}
		if status < 500 && httpStatus != status {
			t.Errorf("status %d was rewritten to %d", status, httpStatus)
		}
		if status >= 500 && httpStatus != http.StatusServiceUnavailable {
			t.Errorf("server-side status %d -> %d, want 503", status, httpStatus)
		}
	}

	body := Fault(FaultClient, "soap_decode", "the request body is not a parseable SOAP envelope", "d-1")
	if !strings.HasPrefix(string(body), xml.Header) {
		t.Error("the fault is missing an XML declaration")
	}
	for _, want := range []string{
		"soapenv:Fault", "soapenv:Client", "soap_decode", "d-1",
		"<usslp:retryable>false</usslp:retryable>",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("fault does not contain %q:\n%s", want, body)
		}
	}
	serverFault := string(Fault(FaultServer, "publish_failed", "could not durably record", "d-2"))
	if !strings.Contains(serverFault, "<usslp:retryable>true</usslp:retryable>") {
		t.Errorf("a server fault must advertise itself as retryable:\n%s", serverFault)
	}

	// The fault must be parseable by a real SOAP client.
	var probe struct {
		XMLName xml.Name
		Fault   struct {
			Code   string `xml:"faultcode"`
			String string `xml:"faultstring"`
		} `xml:"Body>Fault"`
	}
	if err := xml.Unmarshal(body, &probe); err != nil {
		t.Fatalf("the generated fault does not parse: %v", err)
	}
	if probe.Fault.Code != string(FaultClient) {
		t.Errorf("round-tripped faultcode = %q", probe.Fault.Code)
	}
}

func TestSOAPSuccessResponse(t *testing.T) {
	body := string(Response("ACCEPTED", "d-1", "corr-1", 3, false))
	for _, want := range []string{
		"PublishItemPriceDescResponse", "ACCEPTED", "d-1", "corr-1",
		"<usslp:changesAccepted>3</usslp:changesAccepted>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("response does not contain %q:\n%s", want, body)
		}
	}
	var probe struct {
		XMLName  xml.Name
		Status   string `xml:"Body>PublishItemPriceDescResponse>status"`
		Accepted int    `xml:"Body>PublishItemPriceDescResponse>changesAccepted"`
	}
	if err := xml.Unmarshal([]byte(body), &probe); err != nil {
		t.Fatalf("the generated response does not parse: %v", err)
	}
	if probe.Status != "ACCEPTED" || probe.Accepted != 3 {
		t.Errorf("round-tripped response = %+v", probe)
	}
}

func TestLegacyCodePageInItemDescriptions(t *testing.T) {
	a := New()
	// A RIB instance on a WE8 database emits ISO-8859-1 the same way SAP does.
	body := `<?xml version="1.0" encoding="ISO-8859-1"?>
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"><soapenv:Body>
  <PublishItemPriceDesc><ItemPriceDesc><store>S1</store><currency_code>EUR</currency_code>
    <ItemPrice><item>A</item><item_desc>Caf` + "\xe9" + ` Cr` + "\xe8" + `me</item_desc>
      <selling_unit_retail>1.00</selling_unit_retail></ItemPrice>
  </ItemPriceDesc></PublishItemPriceDesc>
</soapenv:Body></soapenv:Envelope>`
	changes, err := a.Ingest(context.Background(), testDelivery(t, body, ""))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if got := changes[0].Attributes["description"]; got != "Café Crème" {
		t.Errorf("description = %q", got)
	}
}
