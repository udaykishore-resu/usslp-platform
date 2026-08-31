package mapping

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseSelector(t *testing.T) {
	for _, c := range []struct {
		raw    string
		scopes string
		steps  int
		wild   bool
	}{
		{"$", "record", 0, false},
		{"$.a", "record", 1, false},
		{"$.a.b.c", "record", 3, false},
		{"$.a[2].b", "record", 3, false},
		{`$["odd name"].b`, "record", 2, false},
		{"$$.header.currency", "document", 2, false},
		{"$^.site.code", "group", 2, false},
		{"$.rows[*]", "record", 2, true},
		{"$.merchants[*][*]", "record", 3, true},
	} {
		sel, err := ParseSelector(c.raw)
		if err != nil {
			t.Fatalf("ParseSelector(%q): %v", c.raw, err)
		}
		got := "record"
		if sel.FromRoot() {
			got = "document"
		} else if sel.FromGroup() {
			got = "group"
		}
		if got != c.scopes {
			t.Errorf("%q scope = %s, want %s", c.raw, got, c.scopes)
		}
		if len(sel.steps) != c.steps {
			t.Errorf("%q steps = %d, want %d", c.raw, len(sel.steps), c.steps)
		}
		if sel.HasWildcard() != c.wild {
			t.Errorf("%q wildcard = %v, want %v", c.raw, sel.HasWildcard(), c.wild)
		}
	}
	for _, bad := range []string{"", "a.b", "$.", "$.a[", "$.a[x]", "$.a[-1]", "$x"} {
		if _, err := ParseSelector(bad); !errors.Is(err, ErrDocument) {
			t.Errorf("ParseSelector(%q) err = %v, want ErrDocument", bad, err)
		}
	}
}

func TestSelectorEval(t *testing.T) {
	doc, err := decodeJSON([]byte(`{
	  "header":{"currency":"eur"},
	  "sites":[
	    {"code":"S1","rows":[{"sku":"A"},{"sku":"B"}]},
	    {"code":"S2","rows":[{"sku":"C"}]}
	  ],
	  "byKey":{"m2":[{"sku":"E"}],"m1":[{"sku":"D"}]}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	sel, _ := ParseSelector("$.sites[*].rows[*].sku")
	got := sel.Eval(doc, nil, doc)
	if len(got) != 3 {
		t.Fatalf("wildcard fan-out = %d values, want 3", len(got))
	}
	// Object wildcards must iterate in a stable order, or the same delivery
	// would emit events in a different order on every replay.
	sel2, _ := ParseSelector("$.byKey[*][*].sku")
	vals := sel2.Eval(doc, nil, doc)
	if len(vals) != 2 || vals[0] != "D" || vals[1] != "E" {
		t.Fatalf("object wildcard = %v, want sorted [D E]", vals)
	}
	sel3, _ := ParseSelector("$$.header.currency")
	if v, ok := sel3.One(map[string]any{}, nil, doc); !ok || v != "eur" {
		t.Fatalf("document-scoped selector = %v (ok=%v)", v, ok)
	}
	sel4, _ := ParseSelector("$.missing.deep")
	if len(sel4.Eval(doc, nil, doc)) != 0 {
		t.Error("a missing member must yield no values, not an error")
	}
}

// kassenwerkDoc is a mapping for a POS shape that none of the hand-written
// adapters handles: a fictional "Kassenwerk 4000" ERP that nests price rows
// under site headers, sends integer mantissas with a per-row decimal-shift
// field, and lower-cases its currency codes.
const kassenwerkDoc = `{
  "version": 1,
  "name": "kassenwerk-4000",
  "source_system": "kw4000",
  "verify": {"type":"hmac_sha256","header":"X-KW-Signature","encoding":"hex","prefix":"sha256="},
  "group": "$.kw4000.sites[*]",
  "root": "$.rows[*]",
  "idempotency": ["$$.kw4000.batch.id", "$$.kw4000.batch.generated"],
  "fields": {
    "sku":          {"path":"$.art", "strip_leading_zeros": true, "trim": true},
    "store":        {"path":"$^.siteCode"},
    "currency":     {"path":"$^.cur", "upper": true},
    "price":        {"type":"shifted", "path":"$.amt", "scale_path":"$.exp"},
    "was_price":    {"type":"shifted", "path":"$.prev", "scale_path":"$.exp", "optional": true},
    "effective_at": {"type":"time", "path":"$.from", "layout":"compact_date", "optional": true},
    "promotion_id": {"path":"$.promo", "optional": true},
    "reason":       {"const":"kw4000-nightly"}
  },
  "attributes": {
    "kw_batch": {"path":"$$.kw4000.batch.id"},
    "kw_line":  {"path":"$.line", "type":"int", "optional": true}
  }
}`

const kassenwerkPayload = `{
  "kw4000": {
    "batch": {"id":"B-88231","generated":"20260830T100211"},
    "sites": [
      {"siteCode":"K-01","cur":"kwd","rows":[
        {"line":1,"art":"0000012345","amt":"2499","exp":3,"prev":"2999","from":"20260901","promo":"P7"},
        {"line":2,"art":"0000067890","amt":"7505","exp":4}
      ]},
      {"siteCode":"K-02","cur":"jpy","rows":[
        {"line":1,"art":"0000012345","amt":"24904","exp":2}
      ]}
    ]
  }
}`

func TestApplyNovelShape(t *testing.T) {
	m, err := Compile([]byte(kassenwerkDoc))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	changes, err := m.Apply([]byte(kassenwerkPayload))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(changes) != 3 {
		t.Fatalf("got %d changes, want 3", len(changes))
	}

	// Site K-01 trades in KWD, a three-decimal currency: 2499 shifted by 3 is
	// 2.499 KWD, which is 2499 minor units.
	c0 := changes[0]
	if c0.SKU != "12345" {
		t.Errorf("sku = %q, want the zero-stripped 12345", c0.SKU)
	}
	if c0.StoreID != "K-01" {
		t.Errorf("store = %q, want K-01 from the group scope", c0.StoreID)
	}
	if c0.Price.Currency != "KWD" || c0.Price.Amount != 2499 {
		t.Errorf("price = %+v, want 2499 KWD", c0.Price)
	}
	if c0.Price.String() != "2.499 KWD" {
		t.Errorf("price renders as %q, want %q", c0.Price.String(), "2.499 KWD")
	}
	if c0.WasPrice == nil || c0.WasPrice.Amount != 2999 {
		t.Errorf("was price = %+v, want 2999 KWD", c0.WasPrice)
	}
	if got := c0.EffectiveAt; !got.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("effective_at = %s, want 2026-09-01T00:00:00Z", got)
	}
	if c0.PromotionID != "P7" {
		t.Errorf("promotion = %q", c0.PromotionID)
	}
	if c0.Reason != "kw4000-nightly" {
		t.Errorf("constant injection failed: reason = %q", c0.Reason)
	}
	if c0.Attributes["kw_batch"] != "B-88231" || c0.Attributes["kw_line"] != "1" {
		t.Errorf("attributes = %v", c0.Attributes)
	}
	if c0.SourceSystem != "kw4000" {
		t.Errorf("source_system = %q", c0.SourceSystem)
	}

	// 7505 shifted by 4 is 0.7505 KWD, which rounds half away from zero to 751
	// fils — the boundary case a naive truncation gets wrong.
	if changes[1].Price.Amount != 751 {
		t.Errorf("second row price = %d, want 751 (0.7505 KWD rounded)", changes[1].Price.Amount)
	}

	// Site K-02 trades in JPY, a zero-decimal currency: 24904 shifted by 2 is
	// 249.04 yen, which is 249 yen.
	c2 := changes[2]
	if c2.StoreID != "K-02" {
		t.Errorf("third change store = %q, want K-02", c2.StoreID)
	}
	if c2.Price.Currency != "JPY" || c2.Price.Amount != 249 {
		t.Errorf("JPY price = %+v, want 249 JPY", c2.Price)
	}
	if c2.WasPrice != nil {
		t.Errorf("optional was_price should be absent, got %+v", c2.WasPrice)
	}
}

func TestIdempotencyPartsFromDocument(t *testing.T) {
	m, err := Compile([]byte(kassenwerkDoc))
	if err != nil {
		t.Fatal(err)
	}
	parts := m.IdempotencyParts([]byte(kassenwerkPayload))
	if len(parts) != 2 || parts[0] != "B-88231" || parts[1] != "20260830T100211" {
		t.Fatalf("idempotency parts = %v", parts)
	}
	if got := m.IdempotencyParts([]byte("not json")); got != nil {
		t.Errorf("unparseable body should yield nil parts, got %v", got)
	}
}

func TestCompileRejectsBadDocuments(t *testing.T) {
	cases := map[string]string{
		"missing name":        `{"fields":{"sku":{"path":"$.a"},"price":{"path":"$.b"},"currency":{"const":"USD"}}}`,
		"unknown field":       `{"name":"x","fields":{"prcie":{"path":"$.b"},"sku":{"path":"$.a"},"price":{"path":"$.b"},"currency":{"const":"USD"}}}`,
		"missing price":       `{"name":"x","fields":{"sku":{"path":"$.a"},"currency":{"const":"USD"}}}`,
		"wildcard in field":   `{"name":"x","fields":{"sku":{"path":"$.a[*]"},"price":{"path":"$.b"},"currency":{"const":"USD"}}}`,
		"bad type for field":  `{"name":"x","fields":{"sku":{"path":"$.a","type":"decimal"},"price":{"path":"$.b"},"currency":{"const":"USD"}}}`,
		"scale without type":  `{"name":"x","fields":{"sku":{"path":"$.a"},"price":{"path":"$.b","scale":2},"currency":{"const":"USD"}}}`,
		"unknown verify type": `{"name":"x","verify":{"type":"magic"},"fields":{"sku":{"path":"$.a"},"price":{"path":"$.b"},"currency":{"const":"USD"}}}`,
		"bad layout":          `{"name":"x","fields":{"sku":{"path":"$.a"},"price":{"path":"$.b"},"currency":{"const":"USD"},"effective_at":{"type":"time","path":"$.c","layout":"nonsense"}}}`,
		"unknown key":         `{"name":"x","wat":1,"fields":{"sku":{"path":"$.a"},"price":{"path":"$.b"},"currency":{"const":"USD"}}}`,
		"group in root":       `{"name":"x","root":"$^.rows[*]","fields":{"sku":{"path":"$.a"},"price":{"path":"$.b"},"currency":{"const":"USD"}}}`,
		"group in idem":       `{"name":"x","idempotency":["$^.id"],"fields":{"sku":{"path":"$.a"},"price":{"path":"$.b"},"currency":{"const":"USD"}}}`,
		"bad version":         `{"version":7,"name":"x","fields":{"sku":{"path":"$.a"},"price":{"path":"$.b"},"currency":{"const":"USD"}}}`,
	}
	for name, doc := range cases {
		if _, err := Compile([]byte(doc)); !errors.Is(err, ErrDocument) {
			t.Errorf("%s: err = %v, want ErrDocument", name, err)
		}
	}
}

func TestApplyPayloadFailures(t *testing.T) {
	m, err := Compile([]byte(`{
	  "name":"strict",
	  "root":"$.rows[*]",
	  "fields":{
	    "sku":{"path":"$.sku"},
	    "price":{"path":"$.price"},
	    "currency":{"const":"USD"}
	  }
	}`))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"not json":       `{`,
		"no rows":        `{"rows":[]}`,
		"missing sku":    `{"rows":[{"price":"1.00"}]}`,
		"missing price":  `{"rows":[{"sku":"A"}]}`,
		"price not text": `{"rows":[{"sku":"A","price":{"nested":1}}]}`,
		"bad decimal":    `{"rows":[{"sku":"A","price":"one dollar"}]}`,
	} {
		if _, err := m.Apply([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if !errors.Is(err, ErrPayload) {
			t.Errorf("%s: err = %v, want ErrPayload", name, err)
		}
	}
}

func TestApplyMinorUnitsAndValueMap(t *testing.T) {
	m, err := Compile([]byte(`{
	  "name":"minor",
	  "root":"$.items[*]",
	  "fields":{
	    "sku":{"path":"$.id"},
	    "price":{"type":"minor_units","path":"$.cents"},
	    "currency":{"path":"$.cur","map":{"dollars":"USD","yen":"JPY"}},
	    "store":{"path":"$.shop","default":"S-1"}
	  }
	}`))
	if err != nil {
		t.Fatal(err)
	}
	changes, err := m.Apply([]byte(`{"items":[
	  {"id":"A","cents":249,"cur":"dollars"},
	  {"id":"B","cents":250,"cur":"yen","shop":"S-9"}
	]}`))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if changes[0].Price.Amount != 249 || changes[0].Price.Currency != "USD" {
		t.Errorf("first = %+v", changes[0].Price)
	}
	if changes[0].StoreID != "S-1" {
		t.Errorf("default not applied: store = %q", changes[0].StoreID)
	}
	if changes[1].Price.Currency != "JPY" || changes[1].StoreID != "S-9" {
		t.Errorf("second = %+v store %q", changes[1].Price, changes[1].StoreID)
	}
	// An unmapped value must fail rather than pass through: an unrecognised
	// currency code reaching a shelf is worse than a quarantined delivery.
	if _, err := m.Apply([]byte(`{"items":[{"id":"C","cents":1,"cur":"euros"}]}`)); err == nil ||
		!strings.Contains(err.Error(), "mapping table") {
		t.Errorf("unmapped value err = %v", err)
	}
}

func TestApplyNoRootTreatsDocumentAsOneRecord(t *testing.T) {
	m, err := Compile([]byte(`{
	  "name":"single",
	  "fields":{"sku":{"path":"$.sku"},"price":{"path":"$.price"},"currency":{"const":"GBP"}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	changes, err := m.Apply([]byte(`{"sku":"ONE","price":"9.99"}`))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(changes) != 1 || changes[0].Price.Amount != 999 {
		t.Fatalf("changes = %+v", changes)
	}
}

func TestTimeLayoutsAndOffset(t *testing.T) {
	m, err := Compile([]byte(`{
	  "name":"times",
	  "root":"$.rows[*]",
	  "fields":{
	    "sku":{"path":"$.sku"},
	    "price":{"path":"$.price"},
	    "currency":{"const":"USD"},
	    "effective_at":{"type":"time","path":"$.from","layouts":["compact_date","date","unix"],"offset_minutes":-300}
	  }
	}`))
	if err != nil {
		t.Fatal(err)
	}
	changes, err := m.Apply([]byte(`{"rows":[
	  {"sku":"A","price":"1.00","from":"20260901"},
	  {"sku":"B","price":"1.00","from":"2026-09-02"},
	  {"sku":"C","price":"1.00","from":"1756684800"}
	]}`))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// A layout with no zone is read at the configured fixed offset, so midnight
	// in a UTC-5 estate is 05:00 UTC, not midnight UTC.
	if got := changes[0].EffectiveAt; !got.Equal(time.Date(2026, 9, 1, 5, 0, 0, 0, time.UTC)) {
		t.Errorf("compact_date with offset = %s", got)
	}
	if got := changes[1].EffectiveAt; !got.Equal(time.Date(2026, 9, 2, 5, 0, 0, 0, time.UTC)) {
		t.Errorf("date with offset = %s", got)
	}
	// An epoch timestamp is absolute and must ignore the offset entirely.
	if got := changes[2].EffectiveAt; !got.Equal(time.Unix(1756684800, 0).UTC()) {
		t.Errorf("unix = %s", got)
	}
}
