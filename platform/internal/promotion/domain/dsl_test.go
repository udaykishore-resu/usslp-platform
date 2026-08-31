package domain

import (
	"strings"
	"testing"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// validDoc is a well-formed promotion document, used as the base for the
// rejection cases so that each one differs in exactly one way.
const validDoc = `{
  "id": "promo-001",
  "tenant_id": "acme",
  "name": "20% off dairy",
  "type": "PERCENTAGE_OFF",
  "priority": 100,
  "stackable": false,
  "params": {"percent_off": 20, "currency": "GBP"},
  "conditions": {"categories": ["dairy"], "min_inventory": 5},
  "display": {"led_color": "RED", "badge": "20% OFF", "show_original_price": true, "animation": "PULSE_BORDER"},
  "schedule": {"start_local": "2026-03-02T00:00", "end_local": "2026-03-09T00:00"},
  "funding": "retailer"
}`

func TestParseRuleAcceptsAWellFormedDocument(t *testing.T) {
	r, err := ParseRule([]byte(validDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.ID != "promo-001" || r.Type != TypePercentageOff || r.Priority != 100 {
		t.Errorf("parsed %+v", r)
	}
	if r.Params.PercentOff != 20 {
		t.Errorf("percent_off = %v", r.Params.PercentOff)
	}
	if !r.Display.ShowOriginalPrice {
		t.Error("show_original_price did not survive")
	}
	if r.Display.LEDColor != "RED" {
		t.Errorf("led_color = %q", r.Display.LEDColor)
	}
}

func TestParseRuleNormalisesEnumCase(t *testing.T) {
	doc := strings.Replace(validDoc, `"led_color": "RED"`, `"led_color": "red"`, 1)
	doc = strings.Replace(doc, `"animation": "PULSE_BORDER"`, `"animation": "pulse_border"`, 1)
	doc = strings.Replace(doc, `"currency": "GBP"`, `"currency": "gbp"`, 1)
	r, err := ParseRule([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Display.LEDColor != "RED" || r.Display.Animation != "PULSE_BORDER" || r.Params.Currency != "GBP" {
		t.Errorf("enums were not normalised: %+v", r.Display)
	}
}

func TestParseRuleRejectsMalformedDocuments(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		mustSay string
	}{
		{"not JSON at all", `{`, ""},
		{"unknown field", strings.Replace(validDoc, `"funding"`, `"fundng"`, 1), ""},
		{"missing id", strings.Replace(validDoc, `"id": "promo-001"`, `"id": ""`, 1), "missing id"},
		{"id with a reserved character",
			strings.Replace(validDoc, `"promo-001"`, `"promo/001"`, 1), "reserved characters"},
		{"unknown type", strings.Replace(validDoc, `"PERCENTAGE_OFF"`, `"FREE_STUFF"`, 1), "unknown type"},
		{"percentage out of range",
			strings.Replace(validDoc, `"percent_off": 20`, `"percent_off": 120`, 1), "outside (0, 100)"},
		{"percentage of zero",
			strings.Replace(validDoc, `"percent_off": 20`, `"percent_off": 0`, 1), "outside (0, 100)"},
		{"percentage rule carrying a fixed price",
			strings.Replace(validDoc, `"percent_off": 20`, `"percent_off": 20, "fixed_price_minor": 199`, 1),
			"must not carry"},
		{"negative priority",
			strings.Replace(validDoc, `"priority": 100`, `"priority": -5`, 1), "outside 0..1000"},
		{"unknown LED colour",
			strings.Replace(validDoc, `"led_color": "RED"`, `"led_color": "MAUVE"`, 1), "not a colour"},
		{"unimplemented animation",
			strings.Replace(validDoc, `"PULSE_BORDER"`, `"EXPLODE"`, 1), "not implemented"},
		{"badge too long for the smallest display",
			strings.Replace(validDoc, `"20% OFF"`, `"TWENTY PER CENT OFF EVERYTHING"`, 1), "characters"},
		{"end before start",
			strings.Replace(validDoc, `"end_local": "2026-03-09T00:00"`, `"end_local": "2026-03-01T00:00"`, 1),
			"must be after"},
		{"no schedule window at all",
			strings.Replace(validDoc, `"start_local": "2026-03-02T00:00", "end_local": "2026-03-09T00:00"`, ``, 1),
			"needs either"},
		{"both an absolute and a wall-clock window",
			strings.Replace(validDoc,
				`"start_local": "2026-03-02T00:00"`,
				`"absolute_start": "2026-03-02T00:00:00Z", "absolute_end": "2026-03-09T00:00:00Z", "start_local": "2026-03-02T00:00"`, 1),
			"cannot both apply"},
		{"unknown funding",
			strings.Replace(validDoc, `"retailer"`, `"magic"`, 1), "not one of"},
		{"weekday out of range",
			strings.Replace(validDoc, `"start_local"`, `"days_of_week": [0, 9], "start_local"`, 1), "not 0..6"},
		{"repeated weekday",
			strings.Replace(validDoc, `"start_local"`, `"days_of_week": [1, 1], "start_local"`, 1), "repeats"},
		{"a daily window with only one end",
			strings.Replace(validDoc, `"start_local"`, `"daily_start": "09:00", "start_local"`, 1),
			"must be set together"},
		{"an invalid clock time",
			strings.Replace(validDoc, `"start_local"`,
				`"daily_start": "25:00", "daily_end": "26:00", "start_local"`, 1), "invalid hour"},
		{"an empty price range",
			strings.Replace(validDoc, `"min_inventory": 5`,
				`"min_price_minor": 500, "max_price_minor": 100`, 1), "is empty"},
		{"every included SKU also excluded",
			strings.Replace(validDoc, `"min_inventory": 5`,
				`"include_skus": ["a", "b"], "exclude_skus": ["a", "b"]`, 1), "never apply"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRule([]byte(tt.doc))
			if err == nil {
				t.Fatal("the document was accepted")
			}
			if tt.mustSay != "" && !strings.Contains(err.Error(), tt.mustSay) {
				t.Errorf("error %q does not mention %q", err, tt.mustSay)
			}
		})
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	// An operator fixing an imported document wants the whole list, not one
	// problem per round trip.
	doc := `{
      "id": "", "tenant_id": "", "name": "", "type": "NOPE", "priority": -1,
      "params": {}, "conditions": {}, "display": {"led_color": "PINK"},
      "schedule": {}
    }`
	_, err := ParseRule([]byte(doc))
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"missing id", "missing tenant_id", "missing name", "unknown type", "priority"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error omits %q: %v", want, err)
		}
	}
}

func TestParseRulesReportsEveryBadDocument(t *testing.T) {
	batch := `[` + validDoc + `,` +
		strings.Replace(validDoc, `"promo-001"`, `"promo-002"`, 1) + `,` +
		strings.Replace(strings.Replace(validDoc, `"promo-001"`, `"promo-003"`, 1),
			`"percent_off": 20`, `"percent_off": 500`, 1) + `]`
	rules, err := ParseRules([]byte(batch))
	if err == nil {
		t.Fatal("the batch was accepted despite a bad document")
	}
	if len(rules) != 2 {
		t.Errorf("kept %d good rules, want 2", len(rules))
	}
	if !strings.Contains(err.Error(), "[2]") {
		t.Errorf("the error does not identify which document failed: %v", err)
	}
}

func TestEachMechanicValidatesItsOwnParameters(t *testing.T) {
	base := func(typ Type, params string) string {
		return `{"id":"p","tenant_id":"t","name":"n","type":"` + string(typ) + `","params":` + params +
			`,"conditions":{},"display":{},"schedule":{"start_local":"2026-01-01T00:00","end_local":"2026-01-02T00:00"}}`
	}
	tests := []struct {
		name string
		doc  string
		ok   bool
	}{
		{"amount off needs a currency", base(TypeAmountOff, `{"amount_off_minor":50}`), false},
		{"amount off with a currency", base(TypeAmountOff, `{"amount_off_minor":50,"currency":"GBP"}`), true},
		{"fixed price must be positive", base(TypeFixedPrice, `{"fixed_price_minor":0,"currency":"GBP"}`), false},
		{"fixed price", base(TypeFixedPrice, `{"fixed_price_minor":199,"currency":"GBP"}`), true},
		{"buy x get y needs all three numbers",
			base(TypeBuyXGetY, `{"buy_qty":2,"get_qty":1}`), false},
		{"buy x get y",
			base(TypeBuyXGetY, `{"buy_qty":2,"get_qty":1,"get_percent_off":100}`), true},
		{"a bundle needs two SKUs",
			base(TypeBundle, `{"bundle_skus":["a"],"fixed_price_minor":500,"currency":"GBP"}`), false},
		{"a bundle",
			base(TypeBundle, `{"bundle_skus":["a","b"],"fixed_price_minor":500,"currency":"GBP"}`), true},
		{"bundle quantities must line up with the SKUs",
			base(TypeBundle, `{"bundle_skus":["a","b"],"bundle_qty":[1],"fixed_price_minor":500,"currency":"GBP"}`), false},
		{"a threshold needs exactly one bar",
			base(TypeThreshold, `{"threshold_spend_minor":2000,"threshold_qty":3,"percent_off":10}`), false},
		{"a threshold needs a reward",
			base(TypeThreshold, `{"threshold_spend_minor":2000}`), false},
		{"a threshold",
			base(TypeThreshold, `{"threshold_spend_minor":2000,"percent_off":10}`), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseRule([]byte(tt.doc))
			if tt.ok && err != nil {
				t.Errorf("rejected a valid document: %v", err)
			}
			if !tt.ok && err == nil {
				t.Error("accepted an invalid document")
			}
		})
	}
}

func TestRenderSpecCarriesTheDisplayBlock(t *testing.T) {
	r, err := ParseRule([]byte(validDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	spec := r.RenderSpec(true)
	if spec.Badge != "20% OFF" || spec.LEDColor != "RED" || spec.Animation != "PULSE_BORDER" {
		t.Errorf("render spec = %+v", spec)
	}
	if !spec.ShowWas {
		t.Error("show_original_price did not reach the render spec")
	}
	if spec.PartialRefresh {
		t.Error("a promotion must take a full refresh, not a partial one")
	}
	if spec.Template != "promo" {
		t.Errorf("template = %q, want the promo default", spec.Template)
	}
	// A rule that does not ask for the was-price must not get one even when the
	// caller offers it, since displaying a saving that was not authorised is
	// the regulated direction of the error.
	r.Display.ShowOriginalPrice = false
	if r.RenderSpec(true).ShowWas {
		t.Error("show_was was set despite the rule not asking for it")
	}
	_ = canon.RenderSpec{}
}
