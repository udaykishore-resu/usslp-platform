// Package mapping implements USSLP's declarative POS mapping documents: the
// mechanism by which a point-of-sale system that nobody has ever integrated
// before becomes a supported source of price changes without a line of Go being
// written, compiled or deployed.
//
// This is the concrete form of the platform's "auto-adapter generation" claim.
// The hand-written adapters exist because Shopify, Square, NCR, SAP and Oracle
// each carry a quirk that is cheaper to express in code than in configuration —
// an HMAC computed over the notification URL, an IDoc in a legacy code page, a
// SOAP fault contract. Everything else, which in a long tail of retail is most
// things, is a JSON payload with fields in unfamiliar places. For those, a
// mapping document is the adapter:
//
//	{
//	  "name": "acme-erp",
//	  "source_system": "acme-erp-v4",
//	  "root": "$.payload.priceRows[*]",
//	  "verify": {"type":"hmac_sha256","header":"X-Acme-Signature","encoding":"hex","prefix":"sha256="},
//	  "idempotency": ["$$.payload.batchId", "$$.payload.generatedAt"],
//	  "fields": {
//	    "sku":          {"path":"$.article", "strip_leading_zeros": true},
//	    "store":        {"path":"$$.payload.site"},
//	    "currency":     {"path":"$$.payload.cur", "upper": true},
//	    "price":        {"type":"shifted", "path":"$.amt", "scale_path":"$.amtExp"},
//	    "effective_at": {"type":"time", "path":"$.from", "layout":"compact_date"}
//	  }
//	}
//
// The document is compiled once, at binding-load time, into a validated
// structure: every selector is parsed, every field name is checked against the
// canonical set, every type coercion is checked against the field it feeds. A
// typo is a configuration error reported when the binding is installed, not a
// silently dropped price at midnight.
//
// What a mapping document deliberately cannot do is compute. There are no
// expressions, no conditionals and no scripting. A partner-supplied document is
// untrusted input on the pricing path, and the only safe amount of programmable
// surface there is none.
package mapping

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/decimal"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// ErrDocument marks a malformed mapping document. It is always an operator
// error, surfaced when a binding is installed so a bad document can never reach
// the price path.
var ErrDocument = errors.New("uig/mapping: invalid mapping document")

// ErrPayload marks a payload that the document cannot be applied to: bad JSON,
// a missing required field, a value that will not coerce. It is a permanent,
// client-side failure — the same bytes will fail identically forever — so the
// pipeline quarantines it and answers 4xx instead of inviting a retry.
var ErrPayload = errors.New("uig/mapping: payload does not match mapping")

// Canonical field names a mapping document may bind. The set is closed: an
// unrecognised key is a compile error rather than a silently ignored line,
// because the failure mode of a silently ignored "prcie" key is a shelf that
// never updates and nobody knowing why.
const (
	FieldSKU         = "sku"
	FieldStore       = "store"
	FieldCurrency    = "currency"
	FieldPrice       = "price"
	FieldWasPrice    = "was_price"
	FieldUnitPrice   = "unit_price"
	FieldUnitMeasure = "unit_measure"
	FieldEffectiveAt = "effective_at"
	FieldExpiresAt   = "expires_at"
	FieldPromotionID = "promotion_id"
	FieldReason      = "reason"
)

// Coercion types a field may declare.
const (
	// TypeString is the default: whatever is at the path, rendered as text.
	TypeString = "string"
	// TypeDecimal reads a decimal string ("2.49") and converts it to exact
	// minor units using the record's currency.
	TypeDecimal = "decimal"
	// TypeMinorUnits reads an integer that is already counted in minor units,
	// which is how Square, Clover and most modern APIs send money.
	TypeMinorUnits = "minor_units"
	// TypeShifted reads an integer mantissa and a separate decimal-shift count,
	// which is how SAP-derived and mainframe-derived feeds send money.
	TypeShifted = "shifted"
	// TypeTime parses a timestamp with a declared layout.
	TypeTime = "time"
	// TypeInt reads an integer, used for attributes and shift fields.
	TypeInt = "int"
)

// Named time layouts, so a mapping document does not have to contain a Go
// reference-time incantation that an integration engineer will get wrong.
var namedLayouts = map[string]string{
	"rfc3339":          time.RFC3339,
	"rfc3339nano":      time.RFC3339Nano,
	"date":             "2006-01-02",
	"datetime":         "2006-01-02 15:04:05",
	"compact_date":     "20060102",
	"compact_datetime": "20060102150405",
	"slash_date":       "01/02/2006",
	"euro_date":        "02/01/2006",
}

// Pseudo-layouts for epoch timestamps.
const (
	layoutUnix   = "unix"
	layoutUnixMS = "unix_ms"
)

// Field describes how one canonical field is read out of a payload.
type Field struct {
	// Path is the selector for the value. It is empty exactly when Const is
	// set, which is how a mapping injects a value the source never sends —
	// a fixed currency for a single-country retailer, or a reason code that
	// marks every row of a nightly feed as an ERP sync.
	Path string `json:"path,omitempty"`
	// Const is a literal value injected instead of reading the payload.
	Const string `json:"const,omitempty"`
	// Type is one of the Type* constants; empty means TypeString.
	Type string `json:"type,omitempty"`
	// Default is used when the path matches nothing. Supplying a default is
	// what makes a field optional in practice.
	Default string `json:"default,omitempty"`
	// Optional allows the field to be absent with no default. Only meaningful
	// for fields that the canonical event itself treats as optional.
	Optional bool `json:"optional,omitempty"`
	// Trim strips surrounding whitespace, which fixed-width-derived JSON feeds
	// need on every single field.
	Trim bool `json:"trim,omitempty"`
	// Upper upper-cases the value. Currency codes arrive lower-cased often
	// enough to be worth a flag.
	Upper bool `json:"upper,omitempty"`
	// StripLeadingZeros removes left padding from an identifier. SAP material
	// numbers and AS/400 item codes are zero-padded to a fixed width, and the
	// retailer's own catalogue is not.
	StripLeadingZeros bool `json:"strip_leading_zeros,omitempty"`
	// Scale is the decimal shift for TypeShifted when the source sends a fixed
	// shift rather than a per-record one.
	Scale int `json:"scale,omitempty"`
	// ScalePath selects a per-record decimal shift for TypeShifted.
	ScalePath string `json:"scale_path,omitempty"`
	// Layout is a named layout or a Go reference-time layout for TypeTime.
	Layout string `json:"layout,omitempty"`
	// Layouts is tried in order when a feed is inconsistent about its
	// timestamps, which nightly exports assembled by several teams often are.
	Layouts []string `json:"layouts,omitempty"`
	// OffsetMinutes is the fixed UTC offset applied to a layout that carries no
	// zone. A fixed offset rather than an IANA location name is deliberate: the
	// UIG runs in distroless containers with no tzdata, and a mapping that
	// silently fell back to UTC when a zone database was missing would shift
	// every promotion start by hours without failing anything.
	OffsetMinutes int `json:"offset_minutes,omitempty"`
	// Map translates source values to canonical ones, e.g. a store code table
	// or a reason-code vocabulary. A value absent from a non-empty table is an
	// error, not a pass-through: silent pass-through is how an unmapped code
	// reaches a shelf.
	Map map[string]string `json:"map,omitempty"`

	sel      Selector
	scaleSel Selector
	layouts  []string
}

// VerifySpec describes how deliveries for this mapping are authenticated.
type VerifySpec struct {
	// Type is "hmac_sha256", "shared_secret" or "none".
	Type string `json:"type"`
	// Header carries the signature or the shared secret.
	Header string `json:"header,omitempty"`
	// Encoding is "hex" or "base64" for hmac_sha256.
	Encoding string `json:"encoding,omitempty"`
	// Prefix is stripped from the header value before comparison, for the
	// common "sha256=..." convention.
	Prefix string `json:"prefix,omitempty"`
	// SignURL includes the request URL ahead of the body in the signed string,
	// the way Square does it.
	SignURL bool `json:"sign_url,omitempty"`
}

// Verification type names.
const (
	VerifyHMACSHA256  = "hmac_sha256"
	VerifySharedToken = "shared_secret"
	VerifyNone        = "none"
)

// Document is the on-the-wire form of a mapping.
type Document struct {
	// Version is the document schema version. Only 1 exists; the field is
	// present so that a future incompatible grammar can be introduced without
	// having to guess which dialect an installed binding was written in.
	Version int `json:"version,omitempty"`
	// Name identifies the mapping in logs, metrics and support tickets.
	Name string `json:"name"`
	// SourceSystem is written to every emitted event's SourceSystem so that
	// analytics can attribute a price change to the POS that requested it.
	SourceSystem string `json:"source_system,omitempty"`
	// Group selects an optional enclosing element — a site, plant or store
	// header — that a run of price rows sits under. Fields written with the $^
	// scope resolve against it. Almost every ERP price feed has this shape
	// eventually, and without it a currency declared once per site would have
	// to be repeated on every row.
	Group string `json:"group,omitempty"`
	// Root selects the repeated element that becomes one price change. When a
	// group is declared, Root is evaluated relative to each group. An empty
	// root treats the group (or, with no group, the whole document) as a single
	// record.
	Root string `json:"root,omitempty"`
	// Idempotency selects the values that identify this delivery for the source
	// system. They are evaluated against the document, not the record, because
	// deduplication is per delivery. An empty list makes the pipeline fall back
	// to a digest of the raw body, which is correct but coarser: a source that
	// re-sends a semantically identical batch with a new timestamp will not
	// dedupe.
	Idempotency []string `json:"idempotency,omitempty"`
	// Verify describes authentication. Absent means the binding's transport
	// (mTLS, a private link) is the authentication, which the compiler requires
	// to be stated explicitly rather than inferred from omission.
	Verify *VerifySpec `json:"verify,omitempty"`
	// DecimalFormat is "plain" or "european", selecting the punctuation
	// convention for TypeDecimal fields.
	DecimalFormat string `json:"decimal_format,omitempty"`
	// Fields binds canonical fields.
	Fields map[string]*Field `json:"fields"`
	// Attributes bind arbitrary source values onto the canonical event's
	// Attributes map, which is where anything the platform does not model but
	// an auditor may later ask about goes.
	Attributes map[string]*Field `json:"attributes,omitempty"`
}

// Mapping is a compiled, validated Document.
type Mapping struct {
	doc      Document
	group    Selector
	hasGroup bool
	root     Selector
	hasRoot  bool
	idem     []Selector
	fields   map[string]*Field
	attrs    map[string]*Field
	format   decimal.Format
	attrKey  []string
}

// Compile parses and validates a mapping document.
//
// Everything that can be checked without a payload is checked here: selector
// grammar, closed field-name set, type/field compatibility, layout names,
// verification shape. The point is that installing a broken mapping fails at
// install time, loudly, in front of the person who wrote it — instead of at
// ingest time, quietly, in front of a shopper.
func Compile(raw []byte) (*Mapping, error) {
	var doc Document
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDocument, err)
	}
	return CompileDocument(doc)
}

// CompileDocument validates an already-decoded document. It is the entry point
// for configuration that arrives as Go structs rather than JSON.
func CompileDocument(doc Document) (*Mapping, error) {
	if doc.Version != 0 && doc.Version != 1 {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrDocument, doc.Version)
	}
	if strings.TrimSpace(doc.Name) == "" {
		return nil, fmt.Errorf("%w: missing name", ErrDocument)
	}
	m := &Mapping{doc: doc, fields: map[string]*Field{}, attrs: map[string]*Field{}}

	switch strings.ToLower(doc.DecimalFormat) {
	case "", "plain":
		m.format = decimal.Plain
	case "european":
		m.format = decimal.European
	default:
		return nil, fmt.Errorf("%w: unknown decimal_format %q", ErrDocument, doc.DecimalFormat)
	}

	if doc.Group != "" {
		sel, err := ParseSelector(doc.Group)
		if err != nil {
			return nil, err
		}
		m.group, m.hasGroup = sel, true
	}
	if doc.Root != "" {
		sel, err := ParseSelector(doc.Root)
		if err != nil {
			return nil, err
		}
		if sel.FromGroup() {
			return nil, fmt.Errorf("%w: root selector %q must not use the $^ group scope", ErrDocument, doc.Root)
		}
		m.root, m.hasRoot = sel, true
	}
	for _, s := range doc.Idempotency {
		sel, err := ParseSelector(s)
		if err != nil {
			return nil, err
		}
		if sel.HasWildcard() {
			return nil, fmt.Errorf("%w: idempotency selector %q must not use [*]", ErrDocument, s)
		}
		if sel.FromGroup() {
			return nil, fmt.Errorf("%w: idempotency selector %q must not use the $^ group scope; "+
				"deduplication is per delivery, and a delivery has no single group", ErrDocument, s)
		}
		m.idem = append(m.idem, sel)
	}
	if v := doc.Verify; v != nil {
		switch v.Type {
		case VerifyHMACSHA256:
			if v.Header == "" {
				return nil, fmt.Errorf("%w: hmac_sha256 verification needs a header", ErrDocument)
			}
			switch v.Encoding {
			case "", "hex", "base64":
			default:
				return nil, fmt.Errorf("%w: unknown signature encoding %q", ErrDocument, v.Encoding)
			}
		case VerifySharedToken:
			if v.Header == "" {
				return nil, fmt.Errorf("%w: shared_secret verification needs a header", ErrDocument)
			}
		case VerifyNone:
		default:
			return nil, fmt.Errorf("%w: unknown verify type %q", ErrDocument, v.Type)
		}
	}

	for name, f := range doc.Fields {
		if !isCanonicalField(name) {
			return nil, fmt.Errorf("%w: unknown field %q", ErrDocument, name)
		}
		if err := compileField(name, f, false); err != nil {
			return nil, err
		}
		m.fields[name] = f
	}
	for name, f := range doc.Attributes {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%w: attribute with empty name", ErrDocument)
		}
		if err := compileField(name, f, true); err != nil {
			return nil, err
		}
		m.attrs[name] = f
		m.attrKey = append(m.attrKey, name)
	}
	sort.Strings(m.attrKey)

	// The three fields without which a price change is not a price change.
	for _, req := range []string{FieldSKU, FieldPrice, FieldCurrency} {
		if _, ok := m.fields[req]; !ok {
			return nil, fmt.Errorf("%w: field %q is required", ErrDocument, req)
		}
	}
	if _, ok := m.fields[FieldStore]; !ok {
		// A binding with a single store may legitimately omit it; the pipeline
		// falls back to the binding's default store. Requiring the mapping to
		// carry it would make single-store integrations impossible to express.
		m.fields[FieldStore] = &Field{Optional: true}
	}
	return m, nil
}

func isCanonicalField(name string) bool {
	switch name {
	case FieldSKU, FieldStore, FieldCurrency, FieldPrice, FieldWasPrice,
		FieldUnitPrice, FieldUnitMeasure, FieldEffectiveAt, FieldExpiresAt,
		FieldPromotionID, FieldReason:
		return true
	}
	return false
}

func compileField(name string, f *Field, attribute bool) error {
	if f == nil {
		return fmt.Errorf("%w: field %q is null", ErrDocument, name)
	}
	if f.Path == "" && f.Const == "" && f.Default == "" && !f.Optional {
		return fmt.Errorf("%w: field %q needs a path, a const or a default", ErrDocument, name)
	}
	if f.Path != "" && f.Const != "" {
		return fmt.Errorf("%w: field %q sets both path and const", ErrDocument, name)
	}
	if f.Path != "" {
		sel, err := ParseSelector(f.Path)
		if err != nil {
			return err
		}
		if sel.HasWildcard() {
			return fmt.Errorf("%w: field %q selector %q must not use [*]; only root may fan out",
				ErrDocument, name, f.Path)
		}
		f.sel = sel
	}
	if f.Type == "" {
		f.Type = defaultTypeFor(name, attribute)
	}
	switch f.Type {
	case TypeString, TypeInt:
	case TypeDecimal, TypeMinorUnits, TypeShifted:
		if attribute {
			return fmt.Errorf("%w: attribute %q cannot use money type %q", ErrDocument, name, f.Type)
		}
		if !isMoneyField(name) {
			return fmt.Errorf("%w: field %q cannot use money type %q", ErrDocument, name, f.Type)
		}
	case TypeTime:
		if !attribute && !isTimeField(name) {
			return fmt.Errorf("%w: field %q cannot use type time", ErrDocument, name)
		}
	default:
		return fmt.Errorf("%w: field %q has unknown type %q", ErrDocument, name, f.Type)
	}
	if isMoneyField(name) && !attribute {
		switch f.Type {
		case TypeDecimal, TypeMinorUnits, TypeShifted:
		default:
			return fmt.Errorf("%w: money field %q cannot use type %q", ErrDocument, name, f.Type)
		}
	}
	if f.Type == TypeShifted {
		if f.ScalePath != "" {
			sel, err := ParseSelector(f.ScalePath)
			if err != nil {
				return err
			}
			if sel.HasWildcard() {
				return fmt.Errorf("%w: field %q scale_path must not use [*]", ErrDocument, name)
			}
			f.scaleSel = sel
		}
	} else if f.ScalePath != "" || f.Scale != 0 {
		return fmt.Errorf("%w: field %q sets a scale but is not type shifted", ErrDocument, name)
	}
	if f.Type == TypeTime {
		layouts := f.Layouts
		if f.Layout != "" {
			layouts = append([]string{f.Layout}, layouts...)
		}
		if len(layouts) == 0 {
			layouts = []string{"rfc3339"}
		}
		for i, l := range layouts {
			if l == layoutUnix || l == layoutUnixMS {
				continue
			}
			if resolved, ok := namedLayouts[l]; ok {
				layouts[i] = resolved
				continue
			}
			// An unnamed layout is taken as a Go reference-time layout. It must
			// both contain at least one time element and round-trip: a layout
			// of pure literal text formats and parses without error while
			// matching nothing, which would silently drop every date in the
			// feed.
			if err := checkGoLayout(l); err != nil {
				return fmt.Errorf("%w: field %q has unusable layout %q: %v", ErrDocument, name, l, err)
			}
		}
		f.layouts = layouts
	}
	return nil
}

// checkGoLayout rejects a layout that Go would accept but that cannot actually
// carry a date.
func checkGoLayout(l string) error {
	ref := time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)
	formatted := ref.Format(l)
	if formatted == l {
		return errors.New("the layout contains no reference-time elements")
	}
	got, err := time.Parse(l, formatted)
	if err != nil {
		return err
	}
	if got.IsZero() {
		return errors.New("the layout parses to the zero time")
	}
	return nil
}

func defaultTypeFor(name string, attribute bool) string {
	if attribute {
		return TypeString
	}
	switch name {
	case FieldPrice, FieldWasPrice, FieldUnitPrice:
		return TypeDecimal
	case FieldEffectiveAt, FieldExpiresAt:
		return TypeTime
	}
	return TypeString
}

func isMoneyField(name string) bool {
	return name == FieldPrice || name == FieldWasPrice || name == FieldUnitPrice
}

func isTimeField(name string) bool {
	return name == FieldEffectiveAt || name == FieldExpiresAt
}

// Name returns the document name.
func (m *Mapping) Name() string { return m.doc.Name }

// SourceSystem returns the source-system label stamped on emitted events.
func (m *Mapping) SourceSystem() string {
	if m.doc.SourceSystem != "" {
		return m.doc.SourceSystem
	}
	return m.doc.Name
}

// Verify returns the verification spec, or nil when the mapping declares none.
func (m *Mapping) Verify() *VerifySpec { return m.doc.Verify }

// IdempotencyParts evaluates the document's idempotency selectors against a
// payload. It returns nil when the document declares none or the payload cannot
// be parsed, leaving the caller to fall back to a body digest — which is always
// correct, just coarser.
func (m *Mapping) IdempotencyParts(body []byte) []string {
	if len(m.idem) == 0 {
		return nil
	}
	doc, err := decodeJSON(body)
	if err != nil {
		return nil
	}
	parts := make([]string, 0, len(m.idem))
	for _, sel := range m.idem {
		v, ok := sel.One(doc, nil, doc)
		if !ok {
			parts = append(parts, "")
			continue
		}
		s, _ := asString(v)
		parts = append(parts, s)
	}
	return parts
}

// Apply turns a payload into canonical price changes.
//
// StoreID is filled with the *source system's* store code rather than a USSLP
// StoreID: resolving an external code to a canonical store is the pipeline's
// enrichment step, and doing it there rather than in each mapping means a
// retailer that renumbers its estate updates one binding rather than every
// mapping document that mentions a store.
func (m *Mapping) Apply(body []byte) ([]canon.PriceChangeRequested, error) {
	doc, err := decodeJSON(body)
	if err != nil {
		return nil, err
	}
	groups := []any{nil}
	if m.hasGroup {
		groups = m.group.Eval(doc, nil, doc)
		if len(groups) == 0 {
			return nil, fmt.Errorf("%w: group selector %q matched nothing", ErrPayload, m.doc.Group)
		}
	}
	var out []canon.PriceChangeRequested
	index := 0
	for _, grp := range groups {
		base := grp
		if base == nil {
			base = doc
		}
		var records []any
		if !m.hasRoot {
			records = []any{base}
		} else {
			records = m.root.Eval(base, grp, doc)
		}
		for _, rec := range records {
			pc, err := m.applyOne(rec, grp, doc)
			if err != nil {
				return nil, fmt.Errorf("record %d: %w", index, err)
			}
			index++
			out = append(out, pc)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: root selector %q matched nothing", ErrPayload, m.doc.Root)
	}
	return out, nil
}

func (m *Mapping) applyOne(rec, grp, doc any) (canon.PriceChangeRequested, error) {
	var pc canon.PriceChangeRequested
	pc.SourceSystem = m.SourceSystem()

	currency, err := m.stringField(FieldCurrency, rec, grp, doc)
	if err != nil {
		return pc, err
	}
	sku, err := m.stringField(FieldSKU, rec, grp, doc)
	if err != nil {
		return pc, err
	}
	pc.SKU = canon.SKU(sku)
	store, err := m.stringField(FieldStore, rec, grp, doc)
	if err != nil {
		return pc, err
	}
	pc.StoreID = canon.StoreID(store)

	price, ok, err := m.moneyField(FieldPrice, currency, rec, grp, doc)
	if err != nil {
		return pc, err
	}
	if !ok {
		return pc, fmt.Errorf("%w: price is required", ErrPayload)
	}
	pc.Price = price

	if was, ok, err := m.moneyField(FieldWasPrice, currency, rec, grp, doc); err != nil {
		return pc, err
	} else if ok {
		pc.WasPrice = &was
	}
	if unit, ok, err := m.moneyField(FieldUnitPrice, currency, rec, grp, doc); err != nil {
		return pc, err
	} else if ok {
		pc.UnitPrice = &unit
	}
	if pc.UnitMeasure, err = m.stringField(FieldUnitMeasure, rec, grp, doc); err != nil {
		return pc, err
	}
	promo, err := m.stringField(FieldPromotionID, rec, grp, doc)
	if err != nil {
		return pc, err
	}
	pc.PromotionID = canon.PromotionID(promo)
	if pc.Reason, err = m.stringField(FieldReason, rec, grp, doc); err != nil {
		return pc, err
	}
	if t, ok, err := m.timeField(FieldEffectiveAt, rec, grp, doc); err != nil {
		return pc, err
	} else if ok {
		pc.EffectiveAt = t
	}
	if t, ok, err := m.timeField(FieldExpiresAt, rec, grp, doc); err != nil {
		return pc, err
	} else if ok {
		exp := t
		pc.ExpiresAt = &exp
	}
	for _, key := range m.attrKey {
		v, ok, err := m.rawField(m.attrs[key], key, rec, grp, doc)
		if err != nil {
			return pc, err
		}
		if !ok || v == "" {
			continue
		}
		if pc.Attributes == nil {
			pc.Attributes = map[string]string{}
		}
		pc.Attributes[key] = v
	}
	return pc, nil
}

// rawField resolves a field to its textual value, applying const injection,
// defaults, trimming, case folding, zero-stripping and value translation.
func (m *Mapping) rawField(f *Field, name string, rec, grp, doc any) (string, bool, error) {
	if f == nil {
		return "", false, nil
	}
	var s string
	found := false
	switch {
	case f.Const != "":
		s, found = f.Const, true
	case f.Path != "":
		if v, ok := f.sel.One(rec, grp, doc); ok {
			str, ok2 := asString(v)
			if !ok2 {
				return "", false, fmt.Errorf("%w: field %q at %s is %T, not a scalar", ErrPayload, name, f.Path, v)
			}
			s, found = str, true
		}
	}
	if !found {
		if f.Default != "" {
			s, found = f.Default, true
		} else if !f.Optional {
			return "", false, fmt.Errorf("%w: field %q not found at %s", ErrPayload, name, f.Path)
		} else {
			return "", false, nil
		}
	}
	if f.Trim {
		s = strings.TrimSpace(s)
	}
	if f.Upper {
		s = strings.ToUpper(s)
	}
	if f.StripLeadingZeros {
		trimmed := strings.TrimLeft(s, "0")
		if trimmed == "" && s != "" {
			// An all-zero identifier is a real value ("0"), not an empty one.
			trimmed = "0"
		}
		s = trimmed
	}
	if len(f.Map) > 0 {
		mapped, ok := f.Map[s]
		if !ok {
			return "", false, fmt.Errorf("%w: field %q value %q is not in the mapping table", ErrPayload, name, s)
		}
		s = mapped
	}
	if f.Type == TypeInt && s != "" {
		if _, err := strconv.ParseInt(s, 10, 64); err != nil {
			return "", false, fmt.Errorf("%w: field %q value %q is not an integer", ErrPayload, name, s)
		}
	}
	return s, true, nil
}

func (m *Mapping) stringField(name string, rec, grp, doc any) (string, error) {
	f, ok := m.fields[name]
	if !ok {
		return "", nil
	}
	s, _, err := m.rawField(f, name, rec, grp, doc)
	return s, err
}

func (m *Mapping) moneyField(name, currency string, rec, grp, doc any) (canon.Money, bool, error) {
	f, ok := m.fields[name]
	if !ok {
		return canon.Money{}, false, nil
	}
	s, found, err := m.rawField(f, name, rec, grp, doc)
	if err != nil || !found || s == "" {
		return canon.Money{}, false, err
	}
	switch f.Type {
	case TypeMinorUnits:
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return canon.Money{}, false, fmt.Errorf("%w: field %q value %q is not an integer minor amount", ErrPayload, name, s)
		}
		mv, err := decimal.FromMinorUnits(n, currency)
		if err != nil {
			return canon.Money{}, false, fmt.Errorf("%w: field %q: %v", ErrPayload, name, err)
		}
		return mv, true, nil
	case TypeShifted:
		shift := f.Scale
		if f.ScalePath != "" {
			v, ok := f.scaleSel.One(rec, grp, doc)
			if !ok {
				return canon.Money{}, false, fmt.Errorf("%w: field %q scale not found at %s", ErrPayload, name, f.ScalePath)
			}
			str, _ := asString(v)
			n, err := strconv.Atoi(strings.TrimSpace(str))
			if err != nil {
				return canon.Money{}, false, fmt.Errorf("%w: field %q scale %q is not an integer", ErrPayload, name, str)
			}
			shift = n
		}
		mv, err := decimal.ShiftedToMinor(s, shift, currency)
		if err != nil {
			return canon.Money{}, false, fmt.Errorf("%w: field %q: %v", ErrPayload, name, err)
		}
		return mv, true, nil
	default:
		mv, err := decimal.ToMinorFormat(s, currency, m.format)
		if err != nil {
			return canon.Money{}, false, fmt.Errorf("%w: field %q: %v", ErrPayload, name, err)
		}
		return mv, true, nil
	}
}

func (m *Mapping) timeField(name string, rec, grp, doc any) (time.Time, bool, error) {
	f, ok := m.fields[name]
	if !ok {
		return time.Time{}, false, nil
	}
	s, found, err := m.rawField(f, name, rec, grp, doc)
	if err != nil || !found || s == "" {
		return time.Time{}, false, err
	}
	loc := time.UTC
	if f.OffsetMinutes != 0 {
		loc = time.FixedZone("cfg", f.OffsetMinutes*60)
	}
	for _, layout := range f.layouts {
		switch layout {
		case layoutUnix:
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				continue
			}
			return time.Unix(n, 0).UTC(), true, nil
		case layoutUnixMS:
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				continue
			}
			return time.UnixMilli(n).UTC(), true, nil
		default:
			if t, err := time.ParseInLocation(layout, s, loc); err == nil {
				return t.UTC(), true, nil
			}
		}
	}
	return time.Time{}, false, fmt.Errorf("%w: field %q value %q matches none of the layouts %v", ErrPayload, name, s, f.layouts)
}

// asString renders a decoded JSON scalar as text without ever passing a number
// through a float. json.Number is already the source's exact digits.
func asString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		return strconv.FormatBool(t), true
	case nil:
		return "", true
	default:
		return "", false
	}
}
