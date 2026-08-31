package filedrop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
)

// csvOptions describes a comma-delimited nightly export with a header row.
const csvOptions = `{
  "format": "delimited",
  "header": "auto",
  "columns": {
    "sku":          {"name":"ITEMCODE", "strip_leading_zeros": true},
    "store":        {"name":"SITE"},
    "currency":     {"name":"CUR", "upper": true},
    "price":        {"name":"PRICE"},
    "was_price":    {"name":"WASPRICE", "optional": true},
    "effective_at": {"name":"FROMDATE", "layout":"compact_date", "optional": true},
    "unit_measure": {"name":"UOM", "optional": true}
  },
  "attributes": {
    "description": {"name":"DESCR", "optional": true}
  }
}`

// fixedOptions describes an AS/400 fixed-width export: columns at character
// offsets, prices with implied decimals, and no header at all.
const fixedOptions = `{
  "format": "fixed",
  "header": "never",
  "encoding": "iso-8859-1",
  "trailer_prefixes": ["TRL"],
  "columns": {
    "sku":          {"offset": 0,  "length": 14, "strip_leading_zeros": true},
    "store":        {"offset": 14, "length": 5},
    "price":        {"offset": 19, "length": 9, "decimals": 2},
    "was_price":    {"offset": 28, "length": 9, "decimals": 2, "optional": true},
    "effective_at": {"offset": 37, "length": 8, "layout":"compact_date", "optional": true},
    "currency":     {"const": "EUR"},
    "reason":       {"const": "as400-nightly"}
  },
  "attributes": {
    "description": {"offset": 45, "length": 20, "optional": true}
  }
}`

func bindWith(t *testing.T, optionsJSON string) *adapter.Binding {
	t.Helper()
	reg := adapter.NewRegistry()
	reg.MustRegister(New())
	store := adapter.NewBindingStore(reg)
	b := &adapter.Binding{
		ID: "nightly", TenantID: "acme", Adapter: Name,
		DefaultCurrency: "EUR",
		Secrets:         adapter.Secrets{HMACKey: "filedrop-upload-secret"},
		Options:         json.RawMessage(optionsJSON),
	}
	if err := store.Put(b); err != nil {
		t.Fatalf("binding: %v", err)
	}
	got, err := store.Get("acme", "nightly")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func fileDelivery(t *testing.T, body []byte, optionsJSON string) *adapter.Delivery {
	t.Helper()
	b := bindWith(t, optionsJSON)
	d := &adapter.Delivery{
		TenantID: "acme", BindingID: b.ID, Binding: b,
		Body:        body,
		ContentType: "text/plain",
		ReceivedAt:  time.Date(2026, 8, 30, 2, 0, 0, 0, time.UTC),
		Headers:     http.Header{},
	}
	d.Headers.Set(HeaderFileName, "PRICE_20260830.txt")
	d.Headers.Set(HeaderFileSHA256, "deadbeef")
	d.Headers.Set(HeaderFileModTime, "2026-08-30T01:55:00Z")
	return d
}

func TestCSVWithHeaderDetection(t *testing.T) {
	a := New()
	body := strings.Join([]string{
		"ITEMCODE,SITE,CUR,PRICE,WASPRICE,FROMDATE,UOM,DESCR",
		"0000123456,0042,eur,12.99,15.50,20260901,EA,Espresso Beans",
		"0000123457,0042,eur,3.75,,,,Loose Tea",
	}, "\n")
	changes, err := a.Ingest(context.Background(), fileDelivery(t, []byte(body), csvOptions))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want 2", len(changes))
	}
	c := changes[0]
	if c.SKU != "123456" || c.StoreID != "0042" {
		t.Errorf("identity = %q @ %q", c.SKU, c.StoreID)
	}
	if c.Price.Amount != 1299 || c.Price.Currency != "EUR" {
		t.Errorf("price = %+v", c.Price)
	}
	if c.WasPrice == nil || c.WasPrice.Amount != 1550 {
		t.Errorf("was = %+v", c.WasPrice)
	}
	if !c.EffectiveAt.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("effective_at = %s", c.EffectiveAt)
	}
	if c.Attributes["description"] != "Espresso Beans" {
		t.Errorf("attributes = %v", c.Attributes)
	}
	if changes[1].WasPrice != nil {
		t.Errorf("an empty optional column produced %+v", changes[1].WasPrice)
	}
}

func TestCSVWithoutHeaderUsesIndexes(t *testing.T) {
	a := New()
	opts := `{
	  "format":"delimited","header":"never","delimiter":"|",
	  "columns":{
	    "sku":{"index":0},"store":{"index":1},"price":{"index":2},"currency":{"const":"GBP"}
	  }}`
	body := "ESP-1KG|0042|12.99\nTEA-500|0042|3.75"
	changes, err := a.Ingest(context.Background(), fileDelivery(t, []byte(body), opts))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(changes) != 2 || changes[0].SKU != "ESP-1KG" || changes[0].Price.Amount != 1299 {
		t.Fatalf("changes = %+v", changes)
	}
	if changes[0].Price.Currency != "GBP" {
		t.Errorf("constant injection failed: %+v", changes[0].Price)
	}
}

func TestOneBadRowInAThousandIsIsolated(t *testing.T) {
	a := New()
	var b strings.Builder
	b.WriteString("ITEMCODE,SITE,CUR,PRICE,WASPRICE,FROMDATE,UOM,DESCR\n")
	for i := 0; i < 1000; i++ {
		if i == 437 {
			// The row every nightly file eventually contains: a price the
			// mainframe wrote as text.
			b.WriteString("0000000437,0042,eur,N/A,,,,Broken Row\n")
			continue
		}
		fmt.Fprintf(&b, "%010d,0042,eur,%d.99,,,,Item %d\n", i, i%50+1, i)
	}
	changes, err := a.Ingest(context.Background(), fileDelivery(t, []byte(b.String()), csvOptions))

	// The whole point of this adapter: 999 shelves update and one does not. A
	// store opening with yesterday's prices on every shelf because of one bad
	// row is the failure this prevents.
	if len(changes) != 999 {
		t.Fatalf("got %d changes, want 999", len(changes))
	}
	partial, ok := adapter.IsPartial(err)
	if !ok {
		t.Fatalf("err = %v, want a PartialError", err)
	}
	if len(partial.Failures) != 1 || partial.Failures[0].Ref != "437" {
		t.Fatalf("failures = %+v", partial.Failures)
	}
	if partial.Total != 1000 {
		t.Errorf("Total = %d, want 1000", partial.Total)
	}
	if partial.Failures[0].Reason != "price_unusable" {
		t.Errorf("reason = %q", partial.Failures[0].Reason)
	}
}

func TestAWhollyMisalignedFileIsRefusedRatherThanPartlyPublished(t *testing.T) {
	a := New()
	var b strings.Builder
	b.WriteString("ITEMCODE,SITE,CUR,PRICE,WASPRICE,FROMDATE,UOM,DESCR\n")
	for i := 0; i < 100; i++ {
		if i%2 == 0 {
			b.WriteString("A,0042,eur,not-a-price,,,,x\n")
		} else {
			fmt.Fprintf(&b, "B%d,0042,eur,1.00,,,,x\n", i)
		}
	}
	changes, err := a.Ingest(context.Background(), fileDelivery(t, []byte(b.String()), csvOptions))
	// Half the rows failing means the columns have shifted, not that a few
	// values are odd. Publishing a mixture of correct and misaligned prices is
	// worse than publishing none.
	if len(changes) != 0 {
		t.Fatalf("changes = %d, want none", len(changes))
	}
	cls := adapter.Classify(err)
	if cls.Reason != "too_many_bad_rows" {
		t.Fatalf("reason = %q", cls.Reason)
	}
	if cls.Kind.HTTPStatus() >= 500 {
		t.Errorf("status %d for an unusable file", cls.Kind.HTTPStatus())
	}
}

func TestFixedWidthAS400Export(t *testing.T) {
	a := New()
	// Columns:      sku(14)      store(5) price(9) was(9)   date(8)   descr(20)
	rows := []string{
		"00000000123456" + "0042 " + "000001299" + "000001550" + "20260901" + "Espresso Beans 1kg  ",
		"00000000123457" + "0042 " + "000000375" + "         " + "        " + "Loose Tea           ",
		"TRL0000002",
	}
	// The description on the first row carries a byte that is only meaningful
	// once the declared code page is applied.
	rows[1] = strings.Replace(rows[1], "Loose Tea", "Th\xe9 Vert", 1)
	body := []byte(strings.Join(rows, "\r\n"))

	changes, err := a.Ingest(context.Background(), fileDelivery(t, body, fixedOptions))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want 2 (the trailer must be dropped)", len(changes))
	}
	c := changes[0]
	if c.SKU != "123456" || c.StoreID != "0042" {
		t.Errorf("identity = %q @ %q", c.SKU, c.StoreID)
	}
	// "000001299" with two implied decimals is 12.99: the point is in the
	// copybook, not in the data.
	if c.Price.Amount != 1299 || c.Price.Currency != "EUR" {
		t.Errorf("price = %+v", c.Price)
	}
	if c.WasPrice == nil || c.WasPrice.Amount != 1550 {
		t.Errorf("was = %+v", c.WasPrice)
	}
	if !c.EffectiveAt.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("effective_at = %s", c.EffectiveAt)
	}
	if c.Reason != "as400-nightly" {
		t.Errorf("constant reason = %q", c.Reason)
	}
	if c.Attributes["description"] != "Espresso Beans 1kg" {
		t.Errorf("description = %q (trailing pad must be trimmed)", c.Attributes["description"])
	}
	if changes[1].Price.Amount != 375 || changes[1].WasPrice != nil {
		t.Errorf("second row = %+v", changes[1])
	}
	if got := changes[1].Attributes["description"]; got != "Thé Vert" {
		t.Errorf("code page not applied: description = %q", got)
	}
}

func TestFixedWidthShortLineIsARowFailureNotAFileFailure(t *testing.T) {
	a := New()
	rows := []string{
		"00000000123456" + "0042 " + "000001299" + "000001550" + "20260901" + "Full row            ",
		"00000000123457" + "0042 ",
		"00000000123458" + "0042 " + "000000375" + "         " + "        " + "Also fine           ",
		"00000000123459" + "0042 " + "000000475" + "         " + "        " + "Also fine           ",
		"00000000123460" + "0042 " + "000000575" + "         " + "        " + "Also fine           ",
	}
	changes, err := a.Ingest(context.Background(), fileDelivery(t, []byte(strings.Join(rows, "\n")), fixedOptions))
	if len(changes) != 4 {
		t.Fatalf("got %d changes, want 4", len(changes))
	}
	partial, ok := adapter.IsPartial(err)
	if !ok || len(partial.Failures) != 1 {
		t.Fatalf("err = %v", err)
	}
}

func TestEuropeanDecimalsAndSkipLines(t *testing.T) {
	a := New()
	opts := `{
	  "format":"delimited","delimiter":";","header":"never","skip_lines":2,
	  "decimal_format":"european",
	  "columns":{"sku":{"index":0},"price":{"index":1},"currency":{"const":"EUR"},"store":{"const":"S1"}}}`
	body := "PREISLISTE 30.08.2026\nErstellt von SAP\nESP-1KG;1.234,56\nTEA-500;3,75"
	changes, err := a.Ingest(context.Background(), fileDelivery(t, []byte(body), opts))
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want 2", len(changes))
	}
	if changes[0].Price.Amount != 123456 {
		t.Errorf("european decimal = %d, want 123456", changes[0].Price.Amount)
	}
	if changes[1].Price.Amount != 375 {
		t.Errorf("second = %d", changes[1].Price.Amount)
	}
}

func TestValueMappingRejectsUnknownCodes(t *testing.T) {
	a := New()
	opts := `{
	  "format":"delimited","header":"never",
	  "columns":{
	    "sku":{"index":0},"price":{"index":1},"currency":{"const":"USD"},
	    "store":{"index":2,"map":{"01":"US-0001","02":"US-0002"}}}}`
	changes, err := a.Ingest(context.Background(),
		fileDelivery(t, []byte("A,1.00,01\nB,2.00,99\nC,3.00,02\nD,4.00,01\nE,5.00,02"), opts))
	if len(changes) != 4 {
		t.Fatalf("got %d changes, want 4", len(changes))
	}
	if changes[0].StoreID != "US-0001" || changes[1].StoreID != "US-0002" {
		t.Errorf("mapped stores = %q, %q", changes[0].StoreID, changes[1].StoreID)
	}
	partial, ok := adapter.IsPartial(err)
	if !ok || partial.Failures[0].Reason != "value_unmapped" {
		t.Fatalf("an unmapped store code was passed through: %v", err)
	}
}

func TestVerifyDistinguishesLocalFilesFromUploads(t *testing.T) {
	a := New()
	ctx := context.Background()

	// A file the local watcher picked up off a mounted share has no HTTP
	// transport at all; its authentication is filesystem permissions.
	local := fileDelivery(t, []byte("x"), csvOptions)
	if err := a.Verify(ctx, local); err != nil {
		t.Fatalf("a locally polled file was rejected: %v", err)
	}

	// A drop uploaded over HTTP is a different proposition and must be signed.
	upload := fileDelivery(t, []byte("x"), csvOptions)
	upload.Method = http.MethodPost
	if err := a.Verify(ctx, upload); err == nil {
		t.Fatal("an unsigned HTTP upload was accepted")
	}
	upload.Headers.Set(HeaderSignature,
		adapter.EncodeSignature(adapter.SignHMACSHA256("filedrop-upload-secret", upload.Body), adapter.EncodingHex))
	if err := a.Verify(ctx, upload); err != nil {
		t.Fatalf("a signed upload was rejected: %v", err)
	}
}

func TestIdempotencyUsesNameAndDigest(t *testing.T) {
	a := New()
	base := fileDelivery(t, []byte("x"), csvOptions)
	parts := a.IdempotencyParts(base)
	if len(parts) != 2 || parts[0] != "file=PRICE_20260830.txt" || parts[1] != "sha256=deadbeef" {
		t.Fatalf("parts = %v", parts)
	}
	// A corrected file re-uploaded under the same name must be processed, so
	// the digest is part of the identity.
	corrected := fileDelivery(t, []byte("x"), csvOptions)
	corrected.Headers.Set(HeaderFileSHA256, "cafebabe")
	if a.IdempotencyParts(corrected)[1] == parts[1] {
		t.Error("the digest is not part of the identity")
	}
	// And yesterday's file copied under a new name must not be reprocessed as
	// something new... it must, in fact, be recognised as different work only
	// if the name differs, which is why both halves are present.
	renamed := fileDelivery(t, []byte("x"), csvOptions)
	renamed.Headers.Set(HeaderFileName, "PRICE_20260829.txt")
	if a.IdempotencyParts(renamed)[0] == parts[0] {
		t.Error("the name is not part of the identity")
	}
	bare := fileDelivery(t, []byte("x"), csvOptions)
	bare.Headers = http.Header{}
	if a.IdempotencyParts(bare) != nil {
		t.Error("with no provenance the adapter must defer to the body digest")
	}
}

func TestUnusableFilesAreQuarantinable(t *testing.T) {
	a := New()
	for name, tc := range map[string]struct{ body, opts string }{
		"empty":        {"", csvOptions},
		"header only":  {"ITEMCODE,SITE,CUR,PRICE,WASPRICE,FROMDATE,UOM,DESCR", csvOptions},
		"bad encoding": {"x", `{"format":"delimited","encoding":"utf-32","columns":{"sku":{"index":0},"price":{"index":1},"currency":{"const":"USD"}}}`},
	} {
		if tc.opts != csvOptions {
			// A binding whose encoding cannot be decoded is refused at install
			// time, which is the earlier and better place to catch it.
			if _, err := (&Adapter{}).CompileOptions(json.RawMessage(tc.opts)); err == nil {
				t.Errorf("%s: bad encoding was accepted at install time", name)
			}
			continue
		}
		_, err := a.Ingest(context.Background(), fileDelivery(t, []byte(tc.body), tc.opts))
		cls := adapter.Classify(err)
		if cls.Kind != adapter.FailureMalformed {
			t.Errorf("%s: kind = %s", name, cls.Kind)
		}
		if cls.Kind.HTTPStatus() >= 500 {
			t.Errorf("%s: status %d", name, cls.Kind.HTTPStatus())
		}
	}
}

func TestCompileOptionsValidatesTheLayout(t *testing.T) {
	a := New()
	for name, opts := range map[string]string{
		"no columns":            `{"format":"delimited"}`,
		"unknown column":        `{"format":"delimited","columns":{"prcie":{"index":0},"sku":{"index":1},"price":{"index":2},"currency":{"const":"USD"}}}`,
		"missing price":         `{"format":"delimited","columns":{"sku":{"index":0},"currency":{"const":"USD"}}}`,
		"fixed without offset":  `{"format":"fixed","columns":{"sku":{"name":"X"},"price":{"offset":0,"length":9},"currency":{"const":"USD"}}}`,
		"offset without length": `{"format":"fixed","columns":{"sku":{"offset":0},"price":{"offset":10,"length":9},"currency":{"const":"USD"}}}`,
		"decimals on a name":    `{"format":"delimited","columns":{"sku":{"index":0,"decimals":2},"price":{"index":1},"currency":{"const":"USD"}}}`,
		"layout on a non-date":  `{"format":"delimited","columns":{"sku":{"index":0,"layout":"date"},"price":{"index":1},"currency":{"const":"USD"}}}`,
		"unknown format":        `{"format":"parquet","columns":{"sku":{"index":0},"price":{"index":1},"currency":{"const":"USD"}}}`,
		"bad ratio":             `{"format":"delimited","max_row_failure_ratio":2,"columns":{"sku":{"index":0},"price":{"index":1},"currency":{"const":"USD"}}}`,
		"unknown key":           `{"format":"delimited","mystery":1,"columns":{"sku":{"index":0},"price":{"index":1},"currency":{"const":"USD"}}}`,
		"relative watch dir":    `{"format":"delimited","watch":{"dir":"drops"},"columns":{"sku":{"index":0},"price":{"index":1},"currency":{"const":"USD"}}}`,
	} {
		if _, err := a.CompileOptions(json.RawMessage(opts)); err == nil {
			t.Errorf("%s: options were accepted", name)
		}
	}
	if _, err := a.CompileOptions(json.RawMessage(csvOptions)); err != nil {
		t.Errorf("a valid layout was rejected: %v", err)
	}
}
