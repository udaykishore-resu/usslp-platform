// Package ncr implements the UIG adapter for NCR item-price messages.
//
// NCR's platform is the one source that pushes the same message in two
// encodings depending on how a site was provisioned: XML from older
// Advanced-Store estates, JSON from anything on the current BSP. Supporting
// both in one adapter — rather than two bindings a retailer has to know to pick
// between — matters because a chain of 900 stores routinely has both, and the
// content type is the only reliable way to tell which store you are hearing
// from.
//
// Authentication is NCR's access-key scheme: an Authorization header carrying a
// key id and a base64 HMAC-SHA256 over a canonical request string. The
// canonical string binds the method, path, content type, date and a digest of
// the body, so a signature captured from one request cannot be replayed against
// another path — and the Date is checked for freshness, which closes the replay
// window that a signature over the body alone would leave open.
package ncr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/decimal"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// Name is the adapter's registered name.
const Name = "ncr"

// Headers NCR's clients send, spelled in net/http's canonical form: a header
// map holds keys canonicalised, so a constant in the wire's own casing would
// miss on lookup.
const (
	HeaderAuthorization = "Authorization"
	HeaderAPIKey        = "X-Ncr-Api-Key"
	HeaderDate          = "Date"
	HeaderOrganization  = "Nep-Organization"
	// HeaderCorrelation is NCR's own correlation id, carried into the canonical
	// event so a retailer's support desk and the platform's are looking at the
	// same identifier.
	HeaderCorrelation = "Nep-Correlation-Id"
)

// utf8BOM is the byte order mark Windows store servers prepend to XML.
const utf8BOM = "\xef\xbb\xbf"

// AuthScheme prefixes the Authorization header value.
const AuthScheme = "AccessKey "

// DefaultClockSkew is how far a signed Date may be from the gateway's clock.
// Fifteen minutes is generous enough for a store server whose NTP has drifted
// and tight enough that a captured request is not replayable for a working day.
const DefaultClockSkew = 15 * time.Minute

// Options is the per-binding configuration.
type Options struct {
	// Organization is the nep-organization value this binding accepts. NCR
	// scopes credentials by organization, and accepting any organization for a
	// valid signature would let one retailer's key post another's prices if the
	// same key were ever shared.
	Organization string `json:"organization,omitempty"`
	// ClockSkew overrides DefaultClockSkew. A zero-length string uses the
	// default; "0s" disables the freshness check for a site whose clock cannot
	// be fixed, which is a deliberate, auditable choice rather than an accident.
	ClockSkew string `json:"clock_skew,omitempty"`
	// PriceModes the adapter acts on. NCR sends cost, list, regular and
	// promotional prices down the same pipe, and only some of them belong on a
	// shelf.
	PriceModes []string `json:"price_modes,omitempty"`

	skew  time.Duration
	modes map[string]bool
}

// DefaultPriceModes are the modes that describe a shelf price.
var DefaultPriceModes = []string{"REGULAR", "PROMOTION", "PROMOTIONAL", "MARKDOWN", "CLEARANCE"}

// Adapter ingests NCR item-price messages.
type Adapter struct{}

// New creates the adapter.
func New() *Adapter { return &Adapter{} }

// Name implements adapter.Adapter.
func (*Adapter) Name() string { return Name }

// CompileOptions validates per-binding options.
func (*Adapter) CompileOptions(raw json.RawMessage) (any, error) {
	opts := &Options{skew: DefaultClockSkew}
	if len(raw) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(opts); err != nil {
			return nil, err
		}
	}
	if opts.ClockSkew != "" {
		d, err := time.ParseDuration(opts.ClockSkew)
		if err != nil {
			return nil, fmt.Errorf("clock_skew %q: %w", opts.ClockSkew, err)
		}
		if d < 0 {
			return nil, fmt.Errorf("clock_skew must not be negative")
		}
		opts.skew = d
	} else {
		opts.skew = DefaultClockSkew
	}
	if len(opts.PriceModes) == 0 {
		opts.PriceModes = DefaultPriceModes
	}
	opts.modes = make(map[string]bool, len(opts.PriceModes))
	for _, m := range opts.PriceModes {
		opts.modes[strings.ToUpper(strings.TrimSpace(m))] = true
	}
	return opts, nil
}

func optionsOf(d *adapter.Delivery) *Options {
	if o, ok := d.Options().(*Options); ok && o != nil {
		return o
	}
	modes := make(map[string]bool, len(DefaultPriceModes))
	for _, m := range DefaultPriceModes {
		modes[m] = true
	}
	return &Options{skew: DefaultClockSkew, modes: modes}
}

// CanonicalString builds the string NCR signs. It is exported because the
// retailer-facing integration guide documents it and the test suite must sign
// requests exactly the way a real client does; a signing rule that only exists
// inside a verifier is a rule nobody can implement against.
func CanonicalString(method, path, contentType, date, organization string, body []byte) string {
	sum := sha256.Sum256(body)
	return strings.Join([]string{
		strings.ToUpper(method),
		path,
		contentType,
		date,
		organization,
		hex.EncodeToString(sum[:]),
	}, "\n")
}

// Verify checks the access-key signature and the Date freshness.
func (*Adapter) Verify(_ context.Context, d *adapter.Delivery) error {
	if accepted, configured := adapter.VerifyPeerIdentity(d.Binding, d.PeerIdentity); configured {
		if accepted {
			return nil
		}
		return adapter.Unauthorized("peer_not_allowed", "client certificate is not in the binding's allow-list")
	}
	opts := optionsOf(d)
	sec := d.Binding.Secrets

	// The api-key header is an identifier, not a credential; it is checked
	// first only so a mis-provisioned client gets a comprehensible error before
	// the signature check produces an opaque one.
	if sec.APIKeyID != "" {
		if err := adapter.VerifySharedToken(sec.APIKeyID, d.Header(HeaderAPIKey), "api key"); err != nil {
			return err
		}
	}
	if opts.Organization != "" {
		if got := d.Header(HeaderOrganization); !strings.EqualFold(got, opts.Organization) {
			return adapter.Unauthorized("wrong_organization",
				"nep-organization does not match the binding's configured organization")
		}
	}
	auth := strings.TrimSpace(d.Header(HeaderAuthorization))
	if !strings.HasPrefix(auth, AuthScheme) {
		return adapter.Unauthorized("missing_signature", "Authorization header is not an NCR AccessKey credential")
	}
	rest := auth[len(AuthScheme):]
	idx := strings.LastIndex(rest, ":")
	if idx <= 0 || idx == len(rest)-1 {
		return adapter.Unauthorized("bad_signature", "AccessKey credential is not <key-id>:<signature>")
	}
	keyID, sig := rest[:idx], rest[idx+1:]
	if sec.APIKeyID != "" && keyID != sec.APIKeyID {
		return adapter.Unauthorized("wrong_key_id", "AccessKey credential names a key id this binding does not accept")
	}

	date := d.Header(HeaderDate)
	if opts.skew > 0 {
		t, err := parseDate(date)
		if err != nil {
			return adapter.Unauthorized("bad_date", "Date header is missing or unparseable, and it is part of the signed string")
		}
		delta := d.ReceivedAt.Sub(t)
		if delta < 0 {
			delta = -delta
		}
		if delta > opts.skew {
			return adapter.Unauthorized("stale_date",
				fmt.Sprintf("signed Date is %s away from the gateway clock, outside the %s window", delta.Round(time.Second), opts.skew))
		}
	}
	canonical := CanonicalString(d.Method, d.Path, d.ContentType, date, d.Header(HeaderOrganization), d.Body)
	return adapter.VerifyHMACSHA256(sec.APIKey, []byte(canonical), sig, adapter.EncodingBase64, "")
}

func parseDate(v string) (time.Time, error) {
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC3339} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparseable date %q", v)
}

// message is the neutral form both encodings decode into.
type message struct {
	XMLName     xml.Name `xml:"ItemPriceMessage" json:"-"`
	MessageID   string   `xml:"MessageId" json:"messageId"`
	SiteID      string   `xml:"SiteId" json:"siteId"`
	Currency    string   `xml:"Currency" json:"currency"`
	GeneratedAt string   `xml:"GeneratedAt" json:"generatedAt"`
	Items       []item   `xml:"Item" json:"items"`
}

type item struct {
	ItemCode      string `xml:"ItemCode" json:"itemCode"`
	Description   string `xml:"Description,omitempty" json:"description,omitempty"`
	PriceMode     string `xml:"PriceMode" json:"priceMode"`
	UnitPrice     string `xml:"UnitPrice" json:"unitPrice"`
	WasPrice      string `xml:"WasPrice,omitempty" json:"wasPrice,omitempty"`
	Currency      string `xml:"Currency,omitempty" json:"currency,omitempty"`
	UnitOfMeasure string `xml:"UnitOfMeasure,omitempty" json:"unitOfMeasure,omitempty"`
	MeasurePrice  string `xml:"MeasurePrice,omitempty" json:"measurePrice,omitempty"`
	EffectiveDate string `xml:"EffectiveDate,omitempty" json:"effectiveDate,omitempty"`
	ExpiryDate    string `xml:"ExpiryDate,omitempty" json:"expiryDate,omitempty"`
	PromotionID   string `xml:"PromotionId,omitempty" json:"promotionId,omitempty"`
	SiteID        string `xml:"SiteId,omitempty" json:"siteId,omitempty"`
}

// IdempotencyParts uses NCR's message id, which a store's outbox replays
// unchanged after a network blip.
func (*Adapter) IdempotencyParts(d *adapter.Delivery) []string {
	m, err := decode(d)
	if err != nil || m.MessageID == "" {
		return nil
	}
	return []string{m.SiteID, m.MessageID}
}

func decode(d *adapter.Delivery) (*message, error) {
	var m message
	if isXML(d) {
		if err := xml.Unmarshal(d.Body, &m); err != nil {
			return nil, adapter.Malformed("xml_decode", "the body is not an NCR ItemPriceMessage", err)
		}
		return &m, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(d.Body)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, adapter.Malformed("json_decode", "the body is not an NCR item price message", err)
	}
	return &m, nil
}

// isXML picks the encoding from the content type, falling back to sniffing the
// first non-space byte. The fallback exists because a non-trivial number of
// store servers post XML with a Content-Type of text/plain, and refusing them
// would be technically correct and commercially useless.
func isXML(d *adapter.Delivery) bool {
	ct := strings.ToLower(d.ContentType)
	switch {
	case strings.Contains(ct, "xml"):
		return true
	case strings.Contains(ct, "json"):
		return false
	}
	// A UTF-8 byte order mark ahead of the document is common from Windows
	// store servers and is not whitespace, so it is skipped explicitly.
	body := strings.TrimPrefix(string(d.Body), utf8BOM)
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case ' ', '\t', '\r', '\n':
			continue
		case '<':
			return true
		default:
			return false
		}
	}
	return false
}

// Ingest converts one NCR message into a price change per item.
func (a *Adapter) Ingest(_ context.Context, d *adapter.Delivery) ([]canon.PriceChangeRequested, error) {
	opts := optionsOf(d)
	m, err := decode(d)
	if err != nil {
		return nil, err
	}
	if len(m.Items) == 0 {
		return nil, adapter.Malformed("no_items", "the message carries no items", nil)
	}
	if t, err := time.Parse(time.RFC3339, m.GeneratedAt); err == nil {
		d.SourceTime = t.UTC()
	}
	msgCurrency := strings.TrimSpace(m.Currency)
	if msgCurrency == "" {
		msgCurrency = d.Binding.Currency()
	}

	out := make([]canon.PriceChangeRequested, 0, len(m.Items))
	var failures []adapter.RowFailure
	for i, it := range m.Items {
		mode := strings.ToUpper(strings.TrimSpace(it.PriceMode))
		if mode != "" && !opts.modes[mode] {
			// A cost or list price is not a shelf price. Skipping it is not a
			// failure and must not show up as one.
			continue
		}
		code := strings.TrimSpace(it.ItemCode)
		if code == "" {
			failures = append(failures, adapter.RowFailure{
				Index: i, Reason: "missing_item_code", Detail: "item carries no ItemCode",
			})
			continue
		}
		cur := strings.TrimSpace(it.Currency)
		if cur == "" {
			cur = msgCurrency
		}
		price, err := decimal.ToMinor(strings.TrimSpace(it.UnitPrice), cur)
		if err != nil {
			failures = append(failures, adapter.RowFailure{
				Index: i, Ref: code, Reason: "price_unusable",
				Detail: fmt.Sprintf("UnitPrice %q: %v", it.UnitPrice, err),
			})
			continue
		}
		site := strings.TrimSpace(it.SiteID)
		if site == "" {
			site = strings.TrimSpace(m.SiteID)
		}
		pc := canon.PriceChangeRequested{
			SKU:          canon.SKU(code),
			StoreID:      canon.StoreID(site),
			Price:        price,
			PromotionID:  canon.PromotionID(strings.TrimSpace(it.PromotionID)),
			UnitMeasure:  strings.TrimSpace(it.UnitOfMeasure),
			SourceSystem: Name,
			Reason:       "ncr:" + strings.ToLower(mode),
			Attributes: map[string]string{
				"ncr_message_id": m.MessageID,
				"ncr_price_mode": mode,
			},
		}
		if it.Description != "" {
			pc.Attributes["description"] = it.Description
		}
		if c := strings.TrimSpace(d.Header(HeaderCorrelation)); c != "" {
			pc.Attributes["ncr_correlation_id"] = c
		}
		if s := strings.TrimSpace(it.WasPrice); s != "" {
			if was, err := decimal.ToMinor(s, cur); err == nil && was.Amount > price.Amount {
				pc.WasPrice = &was
			} else if err != nil {
				failures = append(failures, adapter.RowFailure{
					Index: i, Ref: code, Reason: "was_price_unusable",
					Detail: fmt.Sprintf("WasPrice %q: %v", s, err),
				})
			}
		}
		if s := strings.TrimSpace(it.MeasurePrice); s != "" {
			if up, err := decimal.ToMinor(s, cur); err == nil {
				pc.UnitPrice = &up
			}
		}
		if t, ok := parseTime(it.EffectiveDate); ok {
			pc.EffectiveAt = t
		}
		if t, ok := parseTime(it.ExpiryDate); ok {
			exp := t
			pc.ExpiresAt = &exp
		}
		out = append(out, pc)
	}
	if len(failures) > 0 {
		return out, &adapter.PartialError{Failures: failures, Total: len(m.Items)}
	}
	return out, nil
}

func parseTime(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02", "20060102"} {
		if t, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
