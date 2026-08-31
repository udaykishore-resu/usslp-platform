// Package oracle implements the UIG adapter for Oracle Retail (RIB)
// ItemPriceDesc messages delivered as SOAP over HTTP.
//
// Two decisions in here are worth stating plainly, because both look wrong
// until you know why.
//
// First, the SOAP envelope is parsed with encoding/xml against a hand-written
// struct rather than with generated stubs. Oracle's WSDL describes a message
// far larger than the price path needs, and the fields that matter — item,
// selling unit retail, store, currency, effective date — have been stable
// across RIB releases for a decade. A hand-written subset is inspectable,
// dependency-free and, crucially, tolerant: a retailer on a customised RIB with
// three extra elements is ingested rather than rejected, which a strict
// generated parser would not do.
//
// Second, a rejection is answered with a SOAP Fault on HTTP 400, not on the
// HTTP 500 that SOAP 1.1 nominally prescribes. Oracle's RIB error hospital
// treats a transport 5xx as retryable and a 4xx as terminal, and a message that
// cannot be parsed will never parse. Answering 500 puts an unparseable message
// at the head of a retry queue where it blocks every price behind it — a real
// failure mode with a real name. The Fault itself carries faultcode
// soapenv:Client, which is the part RIB actually routes on.
package oracle

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/codepage"
	"github.com/usslp/usslp/platform/internal/uig/decimal"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// Name is the adapter's registered name.
const Name = "oracle-retail"

// SOAPNamespace is the SOAP 1.1 envelope namespace RIB uses.
const SOAPNamespace = "http://schemas.xmlsoap.org/soap/envelope/"

// PasswordTextType is the WS-Security password type this adapter accepts.
// PasswordDigest is deliberately not accepted: it requires the server to hold
// the password in a recoverable form and buys nothing over a shared secret sent
// inside TLS.
const PasswordTextType = "http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordText"

// Options is the per-binding configuration.
type Options struct {
	// RequireWSSecurity demands a UsernameToken even when the binding also
	// accepts a client certificate. Retailers running RIB over a private
	// interconnect frequently want both.
	RequireWSSecurity bool `json:"require_ws_security,omitempty"`
	// ZeroRetailIsWithdrawal treats a selling_unit_retail of zero as a price
	// withdrawal rather than a price of nothing. RIB estates differ on this and
	// getting it backwards puts "0.00" on a shelf, so it is explicit
	// configuration with a safe default of false — a zero retail is refused
	// unless the retailer says it means withdrawal.
	ZeroRetailIsWithdrawal bool `json:"zero_retail_is_withdrawal,omitempty"`
}

// Adapter ingests Oracle Retail SOAP price messages.
type Adapter struct{}

// New creates the adapter.
func New() *Adapter { return &Adapter{} }

// Name implements adapter.Adapter.
func (*Adapter) Name() string { return Name }

// CompileOptions validates per-binding options.
func (*Adapter) CompileOptions(raw json.RawMessage) (any, error) {
	opts := &Options{}
	if len(raw) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(opts); err != nil {
			return nil, err
		}
	}
	return opts, nil
}

func optionsOf(d *adapter.Delivery) *Options {
	if o, ok := d.Options().(*Options); ok && o != nil {
		return o
	}
	return &Options{}
}

// ---------------------------------------------------------------------------
// Envelope
// ---------------------------------------------------------------------------

type envelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Header  header   `xml:"Header"`
	Body    body     `xml:"Body"`
}

type header struct {
	Security  security   `xml:"Security"`
	RibHeader *ribHeader `xml:"RibMessageHeader"`
}

type security struct {
	UsernameToken usernameToken `xml:"UsernameToken"`
}

type usernameToken struct {
	Username string   `xml:"Username"`
	Password password `xml:"Password"`
	Nonce    string   `xml:"Nonce"`
	Created  string   `xml:"Created"`
}

type password struct {
	Type  string `xml:"Type,attr"`
	Value string `xml:",chardata"`
}

// ribHeader is RIB's own message header. Its messageId is the deduplication
// token, and RIB replays it unchanged when a subscriber does not acknowledge.
type ribHeader struct {
	MessageID   string `xml:"messageId"`
	Family      string `xml:"family"`
	MessageType string `xml:"messageType"`
	PublishTime string `xml:"publishTime"`
}

type body struct {
	// Direct catches installations that put the descriptor straight into the
	// SOAP body with no operation wrapper at all.
	Direct []itemPriceDesc `xml:"ItemPriceDesc"`
	// Publish accepts any operation wrapper — PublishItemPriceDescCreate,
	// PublishItemPriceDescModify and the customer-named variants — because the
	// payload inside them is identical and refusing an unfamiliar wrapper would
	// break a retailer on a renamed service for no gain. Matching by "any"
	// rather than by name is what makes the adapter survive a retailer renaming
	// their service, which they do.
	Publish []publishWrapper `xml:",any"`
}

type publishWrapper struct {
	XMLName xml.Name
	Descs   []itemPriceDesc `xml:"ItemPriceDesc"`
}

// descriptors flattens whichever shape arrived.
func (b body) descriptors() []itemPriceDesc {
	out := make([]itemPriceDesc, 0, len(b.Direct))
	out = append(out, b.Direct...)
	for _, p := range b.Publish {
		out = append(out, p.Descs...)
	}
	return out
}

type itemPriceDesc struct {
	Store        string      `xml:"store"`
	StoreAlt     string      `xml:"loc"`
	CurrencyCode string      `xml:"currency_code"`
	PriceChangeI string      `xml:"price_change_id"`
	Prices       []itemPrice `xml:"ItemPrice"`
}

type itemPrice struct {
	Item              string `xml:"item"`
	ItemParent        string `xml:"item_parent"`
	ItemDesc          string `xml:"item_desc"`
	SellingUnitRetail string `xml:"selling_unit_retail"`
	SellingUOM        string `xml:"selling_uom"`
	MultiUnitRetail   string `xml:"multi_unit_retail"`
	MultiUnits        string `xml:"multi_units"`
	MultiSellingUOM   string `xml:"multi_selling_uom"`
	StandardRetail    string `xml:"standard_unit_retail"`
	CurrencyCode      string `xml:"currency_code"`
	EffectiveDate     string `xml:"effective_date"`
	EndDate           string `xml:"end_date"`
	Promotion         string `xml:"promotion"`
	Loc               string `xml:"loc"`
}

func parseEnvelope(bodyBytes []byte) (*envelope, error) {
	dec := xml.NewDecoder(bytes.NewReader(bodyBytes))
	// RIB installations at retailers with non-English locales emit ISO-8859-1
	// item descriptions for the same reasons SAP does.
	dec.CharsetReader = codepage.Reader
	dec.Strict = false
	var env envelope
	if err := dec.Decode(&env); err != nil {
		return nil, adapter.Malformed("soap_decode", "the request body is not a parseable SOAP envelope", err)
	}
	if env.XMLName.Local != "Envelope" {
		return nil, adapter.Malformed("not_soap",
			fmt.Sprintf("the request root element is <%s>, not a SOAP Envelope", env.XMLName.Local), nil)
	}
	return &env, nil
}

// Verify checks the WS-Security UsernameToken, or accepts a verified mTLS peer.
func (*Adapter) Verify(_ context.Context, d *adapter.Delivery) error {
	opts := optionsOf(d)
	accepted, configured := adapter.VerifyPeerIdentity(d.Binding, d.PeerIdentity)
	if configured && !opts.RequireWSSecurity {
		if accepted {
			return nil
		}
		return adapter.Unauthorized("peer_not_allowed", "client certificate is not in the binding's allow-list")
	}
	if configured && !accepted {
		return adapter.Unauthorized("peer_not_allowed", "client certificate is not in the binding's allow-list")
	}

	env, err := parseEnvelope(d.Body)
	if err != nil {
		// Parsing the envelope is part of authenticating it, since the
		// credential lives inside the document. An unparseable envelope is
		// reported as unauthorized rather than malformed so that an
		// unauthenticated caller cannot use the error to distinguish "your XML
		// is bad" from "your password is bad".
		return adapter.Unauthorized("no_credentials", "the request carried no usable WS-Security header")
	}
	tok := env.Header.Security.UsernameToken
	if tok.Password.Type != "" && tok.Password.Type != PasswordTextType {
		return adapter.Unauthorized("unsupported_password_type",
			"only PasswordText UsernameTokens are accepted")
	}
	if want := d.Binding.Secrets.Username; want != "" {
		if err := adapter.VerifySharedToken(want, strings.TrimSpace(tok.Username), "username"); err != nil {
			return err
		}
	}
	return adapter.VerifySharedToken(d.Binding.Secrets.SharedToken, strings.TrimSpace(tok.Password.Value), "password")
}

// IdempotencyParts uses RIB's messageId, which is stable across the replays the
// error hospital performs.
func (*Adapter) IdempotencyParts(d *adapter.Delivery) []string {
	env, err := parseEnvelope(d.Body)
	if err != nil || env.Header.RibHeader == nil {
		return nil
	}
	h := env.Header.RibHeader
	if strings.TrimSpace(h.MessageID) == "" {
		return nil
	}
	return []string{strings.TrimSpace(h.Family), strings.TrimSpace(h.MessageType), strings.TrimSpace(h.MessageID)}
}

// Ingest converts a SOAP ItemPriceDesc message into canonical price changes.
func (a *Adapter) Ingest(_ context.Context, d *adapter.Delivery) ([]canon.PriceChangeRequested, error) {
	opts := optionsOf(d)
	env, err := parseEnvelope(d.Body)
	if err != nil {
		return nil, err
	}
	descs := env.Body.descriptors()
	if len(descs) == 0 {
		return nil, adapter.Malformed("no_item_price_desc",
			"the SOAP body carries no ItemPriceDesc element", nil)
	}
	if h := env.Header.RibHeader; h != nil {
		if t, ok := parseTime(h.PublishTime); ok {
			d.SourceTime = t
		}
	}

	var out []canon.PriceChangeRequested
	var failures []adapter.RowFailure
	total, index := 0, 0

	for _, desc := range descs {
		store := firstNonEmpty(strings.TrimSpace(desc.Store), strings.TrimSpace(desc.StoreAlt))
		descCurrency := strings.TrimSpace(desc.CurrencyCode)
		for _, ip := range desc.Prices {
			total++
			pos := index
			index++
			item := strings.TrimSpace(ip.Item)
			if item == "" {
				failures = append(failures, adapter.RowFailure{
					Index: pos, Reason: "missing_item", Detail: "ItemPrice carries no <item>",
				})
				continue
			}
			currency := firstNonEmpty(strings.TrimSpace(ip.CurrencyCode), descCurrency, d.Binding.Currency())
			retail := strings.TrimSpace(ip.SellingUnitRetail)
			if retail == "" {
				failures = append(failures, adapter.RowFailure{
					Index: pos, Ref: item, Reason: "missing_retail",
					Detail: "ItemPrice carries no <selling_unit_retail>",
				})
				continue
			}
			price, err := decimal.ToMinor(retail, currency)
			if err != nil {
				failures = append(failures, adapter.RowFailure{
					Index: pos, Ref: item, Reason: "price_unusable",
					Detail: fmt.Sprintf("selling_unit_retail %q in %s: %v", retail, currency, err),
				})
				continue
			}
			if price.Amount == 0 {
				if opts.ZeroRetailIsWithdrawal {
					// A withdrawal, not a price. The shelf keeps what it has.
					continue
				}
				failures = append(failures, adapter.RowFailure{
					Index: pos, Ref: item, Reason: "zero_retail",
					Detail: "selling_unit_retail is zero; set zero_retail_is_withdrawal if this RIB estate means withdrawal",
				})
				continue
			}
			itemStore := firstNonEmpty(strings.TrimSpace(ip.Loc), store)
			pc := canon.PriceChangeRequested{
				SKU:          canon.SKU(item),
				StoreID:      canon.StoreID(itemStore),
				Price:        price,
				UnitMeasure:  strings.TrimSpace(ip.SellingUOM),
				PromotionID:  canon.PromotionID(strings.TrimSpace(ip.Promotion)),
				SourceSystem: Name,
				Reason:       "oracle:item_price_desc",
				Attributes: map[string]string{
					"oracle_item": item,
				},
			}
			if desc.PriceChangeI != "" {
				pc.Attributes["oracle_price_change_id"] = strings.TrimSpace(desc.PriceChangeI)
			}
			if h := env.Header.RibHeader; h != nil && h.MessageID != "" {
				pc.Attributes["rib_message_id"] = strings.TrimSpace(h.MessageID)
			}
			if v := strings.TrimSpace(ip.ItemDesc); v != "" {
				pc.Attributes["description"] = v
			}
			if v := strings.TrimSpace(ip.ItemParent); v != "" {
				pc.Attributes["oracle_item_parent"] = v
			}
			if s := strings.TrimSpace(ip.StandardRetail); s != "" {
				if was, err := decimal.ToMinor(s, currency); err == nil && was.Amount > price.Amount {
					pc.WasPrice = &was
				}
			}
			// A multi-unit retail ("3 for 5.00") is the shelf's unit-price
			// block, not its price: the price is still per selling unit.
			if s := strings.TrimSpace(ip.MultiUnitRetail); s != "" {
				if mu, err := decimal.ToMinor(s, currency); err == nil {
					pc.Attributes["oracle_multi_unit_retail"] = mu.String()
					pc.Attributes["oracle_multi_units"] = strings.TrimSpace(ip.MultiUnits)
				}
			}
			if t, ok := parseTime(ip.EffectiveDate); ok {
				pc.EffectiveAt = t
			}
			if t, ok := parseTime(ip.EndDate); ok {
				exp := t
				pc.ExpiresAt = &exp
			}
			out = append(out, pc)
		}
	}
	if len(failures) > 0 {
		return out, &adapter.PartialError{Failures: failures, Total: total}
	}
	return out, nil
}

func parseTime(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02", "20060102",
	} {
		if t, err := time.ParseInLocation(layout, v, time.UTC); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
