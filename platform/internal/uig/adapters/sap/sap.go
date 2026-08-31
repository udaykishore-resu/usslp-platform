// Package sap implements the UIG adapter for SAP Retail PRICAT IDocs.
//
// This is the hardest source the platform ingests, and the difficulty is not
// the XML. It is that an IDoc is a fixed-width mainframe record wearing an XML
// costume, produced by a system whose retry behaviour makes double-processing
// the default outcome unless you work at preventing it.
//
// Four properties drive the whole implementation:
//
//   - Retries must not double-process. An SAP ALE port resends an IDoc whenever
//     its acknowledgement is slow, and a resent IDoc is byte-identical apart
//     from nothing at all: same DOCNUM, same segments. The deduplication key is
//     therefore the IDoc number, the change action and the creation timestamp,
//     exactly as the platform blueprint requires — the tuple SAP itself treats
//     as the identity of a document.
//   - Segments repeat, at every level. One transmission carries several IDOCs,
//     each with several E1PRHDR header segments (one per plant), each with
//     several E1PRITM item segments. Anything that assumes one of anything is
//     wrong the first time a retailer publishes a price book.
//   - Prices carry an explicit decimal shift. KBETR is an integer condition
//     value and DECSHIFT says where the point goes; the shift is frequently
//     finer than the currency's own exponent because SAP holds sub-cent
//     condition granularity. Nothing here divides in floating point.
//   - Fields are code-page encoded and space padded. ISO-8859-1 or
//     Windows-1252, MATNR left-padded with zeros to eighteen characters, and
//     trailing spaces everywhere. See the codepage package for why the first
//     of those is a correctness issue rather than a nicety.
package sap

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/codepage"
	"github.com/usslp/usslp/platform/internal/uig/decimal"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// Name is the adapter's registered name.
const Name = "sap-idoc"

// Headers an SAP ALE HTTP port sends, spelled in net/http's canonical form.
const (
	// HeaderSignature carries an HMAC over the body when the retailer's
	// middleware signs it. Many SAP estates instead terminate mTLS at the edge,
	// which is why peer identity is an accepted alternative.
	HeaderSignature = "X-Sap-Signature"
	// HeaderDocNum is sent by some middleware layers ahead of the body, and is
	// used only as a cross-check against the document itself.
	HeaderDocNum = "X-Sap-Docnum"
)

// SAP change-action codes carried in E1PRHDR/ACTION.
const (
	// ActionAdd is an original transmission of a new condition record.
	ActionAdd = "009"
	// ActionChange is a change to an existing condition record.
	ActionChange = "004"
	// ActionDelete withdraws a condition record. It is not a price change: the
	// shelf keeps its current price until a replacement condition arrives, and
	// inventing a price for a withdrawal would be a compliance incident.
	ActionDelete = "003"
)

// Options is the per-binding configuration.
type Options struct {
	// DefaultDecimalShift is used when a segment omits DECSHIFT. Two is the SAP
	// default for a two-decimal currency; a retailer whose condition types use
	// a different granularity sets it here.
	DefaultDecimalShift *int `json:"default_decimal_shift,omitempty"`
	// StripMaterialZeros removes SAP's left zero-padding from MATNR. On by
	// default, because a retailer's own catalogue holds "123456" where SAP
	// holds "000000000000123456", and a SKU mismatch means a label that never
	// updates.
	StripMaterialZeros *bool `json:"strip_material_zeros,omitempty"`
	// SKUField selects which item field is the shelf SKU: "MATNR" (default) or
	// "EAN11" for grocery estates that label by barcode.
	SKUField string `json:"sku_field,omitempty"`
	// Actions the adapter acts on. Deletions are excluded by default.
	Actions []string `json:"actions,omitempty"`
	// AllowInexactPriceUnit rounds when a condition's KPEIN pricing unit does
	// not divide its KBETR exactly. Off by default: silently rounding a
	// price-per-thousand condition down to a per-unit price is how a 0.4 cent
	// item becomes free.
	AllowInexactPriceUnit bool `json:"allow_inexact_price_unit,omitempty"`
	// ListPriceCondition is the condition type in E1PRREF that carries the
	// recommended retail price, shown struck through on a promotion label.
	ListPriceCondition string `json:"list_price_condition,omitempty"`

	actions map[string]bool
}

// DefaultActions are the change types that describe a new shelf price.
var DefaultActions = []string{ActionAdd, ActionChange}

// DefaultListPriceCondition is SAP Retail's standard sales-price condition for
// the recommended retail price.
const DefaultListPriceCondition = "VKP0"

// Adapter ingests PRICAT IDocs.
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
	switch strings.ToUpper(strings.TrimSpace(opts.SKUField)) {
	case "", "MATNR":
		opts.SKUField = "MATNR"
	case "EAN11":
		opts.SKUField = "EAN11"
	default:
		return nil, fmt.Errorf("unknown sku_field %q; expected MATNR or EAN11", opts.SKUField)
	}
	if len(opts.Actions) == 0 {
		opts.Actions = DefaultActions
	}
	opts.actions = make(map[string]bool, len(opts.Actions))
	for _, a := range opts.Actions {
		opts.actions[strings.TrimSpace(a)] = true
	}
	if opts.ListPriceCondition == "" {
		opts.ListPriceCondition = DefaultListPriceCondition
	}
	if s := opts.DefaultDecimalShift; s != nil && (*s < -6 || *s > 9) {
		return nil, fmt.Errorf("default_decimal_shift %d is outside the plausible range", *s)
	}
	return opts, nil
}

func optionsOf(d *adapter.Delivery) *Options {
	if o, ok := d.Options().(*Options); ok && o != nil {
		return o
	}
	return &Options{
		SKUField:           "MATNR",
		ListPriceCondition: DefaultListPriceCondition,
		actions:            map[string]bool{ActionAdd: true, ActionChange: true},
	}
}

func (o *Options) decimalShift() int {
	if o.DefaultDecimalShift != nil {
		return *o.DefaultDecimalShift
	}
	return 2
}

func (o *Options) stripZeros() bool {
	return o.StripMaterialZeros == nil || *o.StripMaterialZeros
}

// Verify accepts either a verified mTLS peer or an HMAC over the raw body.
//
// Both are offered because SAP estates split roughly evenly: retailers running
// an ALE HTTP port through their own middleware sign the body, and retailers
// running a direct connection over a private interconnect present a client
// certificate and sign nothing.
func (*Adapter) Verify(_ context.Context, d *adapter.Delivery) error {
	if accepted, configured := adapter.VerifyPeerIdentity(d.Binding, d.PeerIdentity); configured {
		if accepted {
			return nil
		}
		return adapter.Unauthorized("peer_not_allowed", "client certificate is not in the binding's allow-list")
	}
	return adapter.VerifyHMACSHA256(
		d.Binding.Secrets.HMACKey, d.Body, d.Header(HeaderSignature), adapter.EncodingHex, "")
}

// ---------------------------------------------------------------------------
// IDoc structure
// ---------------------------------------------------------------------------

// transmission is the outer element, whose name varies by IDoc type
// (PRICAT01, PRICAT_CREATE01, ZPRICAT01 for a customer extension). An untagged
// XMLName accepts whichever one arrives, which matters because a retailer
// switching to an extended IDoc type must not need a code change.
type transmission struct {
	XMLName xml.Name
	IDocs   []idoc `xml:"IDOC"`
}

type idoc struct {
	Control control `xml:"EDI_DC40"`
	Headers []prhdr `xml:"E1PRHDR"`
}

// control is the EDI_DC40 control record: the identity of the document.
type control struct {
	TabName string `xml:"TABNAM"`
	DocNum  string `xml:"DOCNUM"`
	Status  string `xml:"STATUS"`
	Direct  string `xml:"DIRECT"`
	IDocTyp string `xml:"IDOCTYP"`
	MesTyp  string `xml:"MESTYP"`
	SndPrn  string `xml:"SNDPRN"`
	RcvPrn  string `xml:"RCVPRN"`
	// CreDat and CreTim are SAP's YYYYMMDD / HHMMSS pair. Together with DOCNUM
	// and the change action they are the deduplication identity.
	CreDat string `xml:"CREDAT"`
	CreTim string `xml:"CRETIM"`
	// Serial is SAP's own serialisation field, present on some ports.
	Serial string `xml:"SERIAL"`
}

// prhdr is E1PRHDR: one plant's worth of price conditions.
type prhdr struct {
	Action string  `xml:"ACTION"`
	Werks  string  `xml:"WERKS"`
	Vkorg  string  `xml:"VKORG"`
	Waers  string  `xml:"WAERS"`
	Datab  string  `xml:"DATAB"`
	Datbi  string  `xml:"DATBI"`
	Items  []pritm `xml:"E1PRITM"`
}

// pritm is E1PRITM: one article's condition.
type pritm struct {
	Matnr    string  `xml:"MATNR"`
	Ean11    string  `xml:"EAN11"`
	Maktx    string  `xml:"MAKTX"`
	Kbetr    string  `xml:"KBETR"`
	Konwa    string  `xml:"KONWA"`
	Kpein    string  `xml:"KPEIN"`
	Kmein    string  `xml:"KMEIN"`
	DecShift string  `xml:"DECSHIFT"`
	Datab    string  `xml:"DATAB"`
	Datbi    string  `xml:"DATBI"`
	Aktnr    string  `xml:"AKTNR"`
	Refs     []prref `xml:"E1PRREF"`
}

// prref is E1PRREF: a reference condition, such as the recommended retail price
// a promotion is discounted from.
type prref struct {
	Kschl    string `xml:"KSCHL"`
	Kbetr    string `xml:"KBETR"`
	Konwa    string `xml:"KONWA"`
	DecShift string `xml:"DECSHIFT"`
}

func parseIDocs(body []byte) ([]idoc, error) {
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.CharsetReader = codepage.Reader
	// SAP ports occasionally emit entity references for characters the code
	// page could not represent; refusing to expand unknown entities keeps the
	// decoder from failing on them while ensuring no external entity is ever
	// fetched.
	dec.Strict = false
	var t transmission
	if err := dec.Decode(&t); err != nil {
		return nil, adapter.Malformed("xml_decode", "the body is not a parseable IDoc transmission", err)
	}
	if len(t.IDocs) > 0 {
		return t.IDocs, nil
	}
	// Some middleware strips the transmission wrapper and posts a bare <IDOC>.
	var single idoc
	dec2 := xml.NewDecoder(bytes.NewReader(body))
	dec2.CharsetReader = codepage.Reader
	dec2.Strict = false
	if err := dec2.Decode(&single); err == nil && single.Control.DocNum != "" {
		return []idoc{single}, nil
	}
	return nil, adapter.Malformed("no_idocs", "the transmission contains no IDOC segments", nil)
}

// IdempotencyParts is the tuple SAP itself treats as a document's identity:
// IDoc number, change action and creation timestamp.
//
// Using it — rather than a digest of the body — is what makes an ALE resend
// safe. The resend is byte-identical, so a digest would work too; but a
// retailer's middleware that re-serialises the XML (reordering attributes,
// changing indentation) between the first send and the resend produces
// different bytes for the same document, and only the document identity catches
// that. Getting this wrong means one price decision becoming two shelf changes.
func (*Adapter) IdempotencyParts(d *adapter.Delivery) []string {
	docs, err := parseIDocs(d.Body)
	if err != nil || len(docs) == 0 {
		return nil
	}
	nums := make([]string, 0, len(docs))
	actions := make([]string, 0, len(docs))
	stamps := make([]string, 0, len(docs))
	for _, doc := range docs {
		c := doc.Control
		num := strings.TrimSpace(c.DocNum)
		if num == "" {
			return nil
		}
		nums = append(nums, num)
		stamps = append(stamps, strings.TrimSpace(c.CreDat)+strings.TrimSpace(c.CreTim))
		action := ""
		if len(doc.Headers) > 0 {
			action = strings.TrimSpace(doc.Headers[0].Action)
		}
		if action == "" {
			action = strings.TrimSpace(c.MesTyp)
		}
		actions = append(actions, action)
	}
	return []string{
		"docnum=" + strings.Join(nums, ","),
		"action=" + strings.Join(actions, ","),
		"created=" + strings.Join(stamps, ","),
	}
}

// Ingest converts a PRICAT transmission into canonical price changes.
func (a *Adapter) Ingest(_ context.Context, d *adapter.Delivery) ([]canon.PriceChangeRequested, error) {
	opts := optionsOf(d)
	docs, err := parseIDocs(d.Body)
	if err != nil {
		return nil, err
	}

	var out []canon.PriceChangeRequested
	var failures []adapter.RowFailure
	total := 0
	index := 0

	for _, doc := range docs {
		if t, ok := sapTime(doc.Control.CreDat, doc.Control.CreTim); ok && d.SourceTime.IsZero() {
			d.SourceTime = t
		}
		docnum := strings.TrimSpace(doc.Control.DocNum)
		if len(doc.Headers) == 0 {
			failures = append(failures, adapter.RowFailure{
				Index: index, Ref: docnum, Reason: "no_header_segment",
				Detail: "IDoc " + docnum + " carries no E1PRHDR segment",
			})
			index++
			continue
		}
		for _, hdr := range doc.Headers {
			action := strings.TrimSpace(hdr.Action)
			if action != "" && !opts.actions[action] {
				// A withdrawal or an action this binding does not act on. Not a
				// failure, and the shelf correctly keeps its current price.
				continue
			}
			plant := strings.TrimSpace(hdr.Werks)
			hdrCurrency := strings.TrimSpace(hdr.Waers)
			for _, it := range hdr.Items {
				total++
				pos := index
				index++
				pc, err := a.convert(opts, doc.Control, hdr, it, plant, hdrCurrency, d.Binding.Currency())
				if err != nil {
					cls := adapter.Classify(err)
					failures = append(failures, adapter.RowFailure{
						Index: pos, Ref: strings.TrimSpace(it.Matnr), Reason: cls.Reason, Detail: cls.Detail,
					})
					continue
				}
				out = append(out, pc)
			}
		}
	}

	if len(failures) > 0 {
		return out, &adapter.PartialError{Failures: failures, Total: total}
	}
	return out, nil
}

func (a *Adapter) convert(
	opts *Options,
	ctrl control,
	hdr prhdr,
	it pritm,
	plant, hdrCurrency, fallbackCurrency string,
) (canon.PriceChangeRequested, error) {
	var pc canon.PriceChangeRequested

	sku := strings.TrimSpace(it.Matnr)
	if opts.SKUField == "EAN11" {
		sku = strings.TrimSpace(it.Ean11)
	}
	if opts.stripZeros() && sku != "" {
		trimmed := strings.TrimLeft(sku, "0")
		if trimmed == "" {
			trimmed = "0"
		}
		sku = trimmed
	}
	if sku == "" {
		return pc, adapter.Invalid("missing_material",
			"item segment carries no "+opts.SKUField, nil)
	}

	currency := strings.TrimSpace(it.Konwa)
	if currency == "" {
		currency = hdrCurrency
	}
	if currency == "" {
		currency = fallbackCurrency
	}

	shift := opts.decimalShift()
	if s := strings.TrimSpace(it.DecShift); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return pc, adapter.Invalid("bad_decimal_shift",
				fmt.Sprintf("DECSHIFT %q is not an integer", s), err)
		}
		shift = n
	}
	amount := strings.TrimSpace(it.Kbetr)
	if amount == "" {
		return pc, adapter.Invalid("missing_condition_value", "item segment carries no KBETR", nil)
	}
	price, err := decimal.ShiftedToMinor(amount, shift, currency)
	if err != nil {
		return pc, adapter.Invalid("price_unusable",
			fmt.Sprintf("KBETR %q with DECSHIFT %d in %s: %v", amount, shift, currency, err), err)
	}

	// KPEIN is the pricing unit: KBETR is the price for KPEIN units of KMEIN.
	// The canonical event carries the price of one sales unit, so a pricing
	// unit above one has to be divided out — and if it does not divide exactly,
	// the result would be a price nobody authorised.
	unit := 1
	if s := strings.TrimSpace(it.Kpein); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n == 0 {
			return pc, adapter.Invalid("bad_price_unit",
				fmt.Sprintf("KPEIN %q is not a usable pricing unit", s), err)
		}
		unit = n
	}
	if unit != 1 {
		per := price.Amount / int64(unit)
		if price.Amount%int64(unit) != 0 {
			if !opts.AllowInexactPriceUnit {
				return pc, adapter.Invalid("price_unit_not_exact",
					fmt.Sprintf("KBETR %s for KPEIN %d does not divide into whole minor units; "+
						"set allow_inexact_price_unit to round", price.String(), unit), nil)
			}
			per = roundDiv(price.Amount, int64(unit))
		}
		price.Amount = per
	}

	pc = canon.PriceChangeRequested{
		SKU:          canon.SKU(sku),
		StoreID:      canon.StoreID(plant),
		Price:        price,
		UnitMeasure:  strings.TrimSpace(it.Kmein),
		PromotionID:  canon.PromotionID(strings.TrimSpace(it.Aktnr)),
		SourceSystem: Name,
		Reason:       "sap:pricat:" + strings.TrimSpace(hdr.Action),
		Attributes: map[string]string{
			"sap_docnum":     strings.TrimSpace(ctrl.DocNum),
			"sap_idoctyp":    strings.TrimSpace(ctrl.IDocTyp),
			"sap_mestyp":     strings.TrimSpace(ctrl.MesTyp),
			"sap_sender":     strings.TrimSpace(ctrl.SndPrn),
			"sap_werks":      plant,
			"sap_action":     strings.TrimSpace(hdr.Action),
			"sap_kbetr":      amount,
			"sap_decshift":   strconv.Itoa(shift),
			"sap_price_unit": strconv.Itoa(unit),
		},
	}
	if v := strings.TrimSpace(it.Maktx); v != "" {
		pc.Attributes["description"] = v
	}
	if v := strings.TrimSpace(hdr.Vkorg); v != "" {
		pc.Attributes["sap_vkorg"] = v
	}

	from := firstNonEmpty(strings.TrimSpace(it.Datab), strings.TrimSpace(hdr.Datab))
	if t, ok := sapTime(from, ""); ok {
		pc.EffectiveAt = t
	}
	to := firstNonEmpty(strings.TrimSpace(it.Datbi), strings.TrimSpace(hdr.Datbi))
	if t, ok := sapTime(to, "235959"); ok {
		// SAP's 99991231 means "no end date"; treating it as an expiry would
		// schedule a price to lapse in the year 9999, which is harmless but
		// noisy in every downstream projection.
		if !strings.HasPrefix(to, "9999") {
			exp := t
			pc.ExpiresAt = &exp
		}
	}

	for _, ref := range it.Refs {
		if !strings.EqualFold(strings.TrimSpace(ref.Kschl), opts.ListPriceCondition) {
			continue
		}
		refShift := shift
		if s := strings.TrimSpace(ref.DecShift); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				refShift = n
			}
		}
		refCurrency := strings.TrimSpace(ref.Konwa)
		if refCurrency == "" {
			refCurrency = currency
		}
		was, err := decimal.ShiftedToMinor(strings.TrimSpace(ref.Kbetr), refShift, refCurrency)
		if err == nil && was.Amount > price.Amount {
			pc.WasPrice = &was
		}
	}
	return pc, nil
}

// roundDiv divides half away from zero, matching canon.Money's rounding so a
// price derived here and a discount computed downstream land on the same minor
// unit.
func roundDiv(num, den int64) int64 {
	if den == 0 {
		return 0
	}
	if (num < 0) != (den < 0) {
		return (num - den/2) / den
	}
	return (num + den/2) / den
}

// sapTime parses SAP's YYYYMMDD date and optional HHMMSS time.
//
// SAP dates carry no zone; they are the plant's local business date. The
// gateway interprets them as UTC and records the raw value in the event's
// attributes, because guessing a zone for a plant the UIG has never heard of
// would silently move a promotion start by hours — and an auditor can always
// recover the original from the attribute.
func sapTime(date, timeOfDay string) (time.Time, bool) {
	date = strings.TrimSpace(date)
	if len(date) != 8 || date == "00000000" {
		return time.Time{}, false
	}
	timeOfDay = strings.TrimSpace(timeOfDay)
	if len(timeOfDay) != 6 {
		timeOfDay = "000000"
	}
	t, err := time.ParseInLocation("20060102150405", date+timeOfDay, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
