// Package filedrop implements the UIG adapter for the oldest integration
// pattern in retail: a file on a share.
//
// This adapter is the reason the platform can quote a customer whose pricing
// lives on an AS/400 that nobody has recompiled since 1997. Those systems
// cannot call a webhook and will not learn to; what they can do, reliably,
// every night at 02:00, is write a flat file. Two shapes cover almost all of
// them: delimited (CSV, pipe, tab) and fixed-width, the latter because a
// mainframe report writer emits columns at byte offsets and has no concept of a
// delimiter.
//
// Four behaviours here are the difference between an integration that works and
// one that produces a support ticket every morning:
//
//   - Per-row error isolation. A 40,000-line file with one row containing a
//     stray quote must produce 39,999 price changes, not zero. A store opening
//     with yesterday's prices on every shelf because of one bad row is the
//     failure this adapter is shaped around.
//   - Implied decimals. A fixed-width price field holds "0000249" and means
//     2.49; the point is in the copybook, not in the data. Column configuration
//     carries the decimal count, and conversion is integer arithmetic.
//   - Code pages. These files are EBCDIC-derived, transcoded to ISO-8859-1 on
//     the way out, and never declare it. The binding states the encoding.
//   - Fixed-width offsets are counted in characters, not bytes, and are applied
//     after transcoding — which is only unambiguous because the encodings these
//     systems use are single-byte.
//
// The polling, the processed markers and the quarantine directory live in
// watcher.go; this file is the parser.
package filedrop

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/usslp/usslp/platform/internal/uig/adapter"
	"github.com/usslp/usslp/platform/internal/uig/codepage"
	"github.com/usslp/usslp/platform/internal/uig/decimal"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// Name is the adapter's registered name.
const Name = "filedrop"

// Headers the watcher stamps on a delivery it built from a file. They are the
// only provenance a file has: there is no request, no caller and no signature,
// so the file's own name, size and digest are what identify it.
//
// They are spelled in net/http's canonical form, because http.Header.Get
// canonicalises the key it is given and a constant written in any other casing
// would silently miss when the header was installed with a map literal.
const (
	HeaderFileName    = "X-Usslp-File-Name"
	HeaderFileModTime = "X-Usslp-File-Modtime"
	HeaderFileSHA256  = "X-Usslp-File-Sha256"
	HeaderFileSize    = "X-Usslp-File-Size"
	// HeaderSignature carries a hex HMAC when a retailer uploads a drop over
	// HTTP instead of writing it to a share.
	HeaderSignature = "X-Usslp-Signature"
)

// File formats.
const (
	// FormatDelimited is CSV and its relatives.
	FormatDelimited = "delimited"
	// FormatFixed is fixed-width, which is what an AS/400 report writer emits.
	FormatFixed = "fixed"
)

// Header detection modes.
const (
	// HeaderAuto decides by comparing the first row against the configured
	// column names. It is the default because a retailer's nightly job changes
	// whether it writes a header more often than anyone admits.
	HeaderAuto = "auto"
	// HeaderAlways always treats the first row as a header.
	HeaderAlways = "always"
	// HeaderNever treats the first row as data.
	HeaderNever = "never"
)

// Column describes where one canonical field lives in a row.
type Column struct {
	// Name is the header name for a delimited file.
	Name string `json:"name,omitempty"`
	// Index is the zero-based column number for a delimited file with no
	// header, or as an override when the header names are unusable.
	Index *int `json:"index,omitempty"`
	// Offset and Length locate a field in a fixed-width row, counted in
	// characters from zero.
	Offset *int `json:"offset,omitempty"`
	Length int  `json:"length,omitempty"`
	// Const injects a fixed value for a field the file does not carry — the
	// currency of a single-country retailer, or a reason code marking every row
	// as a nightly sync.
	Const string `json:"const,omitempty"`
	// Default is used when the column is present but empty.
	Default string `json:"default,omitempty"`
	// Optional allows the field to be absent.
	Optional bool `json:"optional,omitempty"`
	// Upper upper-cases the value.
	Upper bool `json:"upper,omitempty"`
	// StripLeadingZeros removes the left padding a mainframe writes on every
	// numeric identifier.
	StripLeadingZeros bool `json:"strip_leading_zeros,omitempty"`
	// Decimals is the implied decimal count for a money column whose file
	// carries no radix point: "0000249" with 2 is 2.49. A nil value means the
	// column carries an explicit decimal point.
	Decimals *int `json:"decimals,omitempty"`
	// Layout is the time layout for a date column, named (as in the mapping
	// package) or a Go reference-time layout.
	Layout string `json:"layout,omitempty"`
	// Map translates source values to canonical ones. A value missing from a
	// non-empty table is an error rather than a pass-through.
	Map map[string]string `json:"map,omitempty"`
}

// Options is the per-binding configuration.
type Options struct {
	// Format is "delimited" or "fixed".
	Format string `json:"format,omitempty"`
	// Delimiter is the field separator for a delimited file. Defaults to a
	// comma; "\t" and "|" are both common.
	Delimiter string `json:"delimiter,omitempty"`
	// Quote is the quoting character; "none" disables quoting entirely, which
	// is what a pipe-delimited mainframe export needs, since a lone double
	// quote inside a product description would otherwise swallow the rest of
	// the file.
	Quote string `json:"quote,omitempty"`
	// Header selects header detection: auto, always or never.
	Header string `json:"header,omitempty"`
	// SkipLines drops a fixed number of leading lines — a banner, a report
	// title, a control record.
	SkipLines int `json:"skip_lines,omitempty"`
	// TrailerPrefixes marks lines to ignore wherever they appear: record-count
	// trailers, page breaks, form feeds.
	TrailerPrefixes []string `json:"trailer_prefixes,omitempty"`
	// Encoding is the file's character encoding: utf-8 (default), iso-8859-1,
	// windows-1252.
	Encoding string `json:"encoding,omitempty"`
	// DecimalFormat is "plain" or "european" for columns with an explicit
	// radix point.
	DecimalFormat string `json:"decimal_format,omitempty"`
	// Columns binds canonical field names to positions.
	Columns map[string]*Column `json:"columns"`
	// Attributes bind extra columns onto the canonical event's Attributes.
	Attributes map[string]*Column `json:"attributes,omitempty"`
	// MaxRowFailureRatio aborts the whole file when more than this fraction of
	// rows fail, as a fraction between 0 and 1. Per-row isolation is right for
	// a handful of bad rows and wrong for a file whose columns have all shifted
	// by one: publishing 12,000 wrong prices is far worse than publishing none.
	// Zero uses DefaultMaxRowFailureRatio.
	MaxRowFailureRatio float64 `json:"max_row_failure_ratio,omitempty"`
	// Watch, when set, makes the service start a directory poller for this
	// binding. Putting it in the binding rather than in the process's own
	// configuration means a new nightly feed is installed the same way every
	// other integration is — one binding — instead of also needing a
	// deployment change to add a directory.
	Watch *WatchSpec `json:"watch,omitempty"`

	format  decimal.Format
	attrKey []string
}

// DefaultMaxRowFailureRatio is the share of rows that may fail before a file is
// treated as structurally wrong rather than merely imperfect.
const DefaultMaxRowFailureRatio = 0.25

// Adapter parses flat-file price drops.
type Adapter struct{}

// New creates the adapter.
func New() *Adapter { return &Adapter{} }

// Name implements adapter.Adapter.
func (*Adapter) Name() string { return Name }

// CompileOptions validates and compiles the column layout.
func (*Adapter) CompileOptions(raw json.RawMessage) (any, error) {
	opts := &Options{}
	if len(raw) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(opts); err != nil {
			return nil, err
		}
	}
	switch strings.ToLower(opts.Format) {
	case "", "csv", FormatDelimited:
		opts.Format = FormatDelimited
	case FormatFixed, "fixed-width", "fixedwidth":
		opts.Format = FormatFixed
	default:
		return nil, fmt.Errorf("unknown format %q; expected delimited or fixed", opts.Format)
	}
	switch strings.ToLower(opts.Header) {
	case "", HeaderAuto:
		opts.Header = HeaderAuto
	case HeaderAlways, HeaderNever, "true", "false":
		if opts.Header == "true" {
			opts.Header = HeaderAlways
		} else if opts.Header == "false" {
			opts.Header = HeaderNever
		}
	default:
		return nil, fmt.Errorf("unknown header mode %q", opts.Header)
	}
	switch strings.ToLower(opts.DecimalFormat) {
	case "", "plain":
		opts.format = decimal.Plain
	case "european":
		opts.format = decimal.European
	default:
		return nil, fmt.Errorf("unknown decimal_format %q", opts.DecimalFormat)
	}
	if _, err := delimiterRune(opts.Delimiter); err != nil {
		return nil, err
	}
	if _, err := quoteRune(opts.Quote); err != nil {
		return nil, err
	}
	if opts.Encoding != "" {
		if _, err := codepage.Decode(opts.Encoding, nil); err != nil {
			return nil, err
		}
	}
	if len(opts.Columns) == 0 {
		return nil, errors.New("a filedrop binding must map at least the sku, price and currency columns")
	}
	for name, c := range opts.Columns {
		if !isCanonicalField(name) {
			return nil, fmt.Errorf("unknown column %q", name)
		}
		if err := validateColumn(name, c, opts.Format); err != nil {
			return nil, err
		}
	}
	for name, c := range opts.Attributes {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("attribute with an empty name")
		}
		if err := validateColumn(name, c, opts.Format); err != nil {
			return nil, err
		}
		opts.attrKey = append(opts.attrKey, name)
	}
	sort.Strings(opts.attrKey)
	for _, req := range []string{FieldSKU, FieldPrice, FieldCurrency} {
		if _, ok := opts.Columns[req]; !ok {
			return nil, fmt.Errorf("column %q is required (a const is fine for currency)", req)
		}
	}
	if opts.MaxRowFailureRatio < 0 || opts.MaxRowFailureRatio > 1 {
		return nil, errors.New("max_row_failure_ratio must be between 0 and 1")
	}
	if opts.MaxRowFailureRatio == 0 {
		opts.MaxRowFailureRatio = DefaultMaxRowFailureRatio
	}
	if opts.Watch != nil {
		if err := opts.Watch.validate(); err != nil {
			return nil, err
		}
	}
	return opts, nil
}

// Canonical column names, matching the mapping package's vocabulary so that a
// retailer moving from a file drop to an API keeps the same field names.
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

func isCanonicalField(name string) bool {
	switch name {
	case FieldSKU, FieldStore, FieldCurrency, FieldPrice, FieldWasPrice,
		FieldUnitPrice, FieldUnitMeasure, FieldEffectiveAt, FieldExpiresAt,
		FieldPromotionID, FieldReason:
		return true
	}
	return false
}

func isMoneyField(name string) bool {
	return name == FieldPrice || name == FieldWasPrice || name == FieldUnitPrice
}

func isTimeField(name string) bool {
	return name == FieldEffectiveAt || name == FieldExpiresAt
}

func validateColumn(name string, c *Column, format string) error {
	if c == nil {
		return fmt.Errorf("column %q is null", name)
	}
	located := c.Name != "" || c.Index != nil || c.Offset != nil
	if !located && c.Const == "" && c.Default == "" && !c.Optional {
		return fmt.Errorf("column %q needs a name, an index, an offset, a const or a default", name)
	}
	if c.Const != "" && located {
		return fmt.Errorf("column %q sets both a const and a position", name)
	}
	if format == FormatFixed && c.Const == "" && c.Offset == nil && !c.Optional {
		return fmt.Errorf("column %q needs an offset in a fixed-width layout", name)
	}
	if c.Offset != nil {
		if *c.Offset < 0 {
			return fmt.Errorf("column %q has a negative offset", name)
		}
		if c.Length <= 0 {
			return fmt.Errorf("column %q has an offset but no length", name)
		}
	}
	if c.Index != nil && *c.Index < 0 {
		return fmt.Errorf("column %q has a negative index", name)
	}
	if c.Decimals != nil {
		if !isMoneyField(name) {
			return fmt.Errorf("column %q is not a money field and cannot declare decimals", name)
		}
		if *c.Decimals < 0 || *c.Decimals > 9 {
			return fmt.Errorf("column %q declares an implausible decimals value %d", name, *c.Decimals)
		}
	}
	if c.Layout != "" && !isTimeField(name) {
		return fmt.Errorf("column %q is not a date field and cannot declare a layout", name)
	}
	if isTimeField(name) {
		if _, err := resolveLayout(c.Layout); err != nil {
			return fmt.Errorf("column %q: %w", name, err)
		}
	}
	return nil
}

var namedLayouts = map[string]string{
	"":                 "2006-01-02",
	"date":             "2006-01-02",
	"rfc3339":          time.RFC3339,
	"compact_date":     "20060102",
	"compact_datetime": "20060102150405",
	"datetime":         "2006-01-02 15:04:05",
	"slash_date":       "01/02/2006",
	"euro_date":        "02/01/2006",
	"julian":           "2006002",
}

func resolveLayout(l string) (string, error) {
	if resolved, ok := namedLayouts[l]; ok {
		return resolved, nil
	}
	// A layout must contain at least one reference-time element. A layout of
	// pure literal text formats and parses without error while matching
	// nothing, which would silently drop every date in the file.
	ref := time.Date(2006, 1, 2, 15, 4, 5, 0, time.UTC)
	formatted := ref.Format(l)
	if formatted == l {
		return "", fmt.Errorf("layout %q contains no reference-time elements", l)
	}
	if _, err := time.Parse(l, formatted); err != nil {
		return "", fmt.Errorf("unusable layout %q", l)
	}
	return l, nil
}

func delimiterRune(s string) (rune, error) {
	switch s {
	case "":
		return ',', nil
	case "\\t", "tab", "\t":
		return '\t', nil
	}
	r := []rune(s)
	if len(r) != 1 {
		return 0, fmt.Errorf("delimiter %q must be a single character", s)
	}
	return r[0], nil
}

func quoteRune(s string) (rune, error) {
	switch strings.ToLower(s) {
	case "":
		return '"', nil
	case "none", "off":
		// encoding/csv disables quoting when LazyQuotes is set and the quote
		// rune is invalid; -1 is how that is expressed.
		return -1, nil
	}
	r := []rune(s)
	if len(r) != 1 {
		return 0, fmt.Errorf("quote %q must be a single character or \"none\"", s)
	}
	return r[0], nil
}

func optionsOf(d *adapter.Delivery) (*Options, error) {
	if o, ok := d.Options().(*Options); ok && o != nil {
		return o, nil
	}
	return nil, adapter.Invalid("no_options", "this filedrop binding has no column layout configured", nil)
}

// Verify authenticates a drop.
//
// A file the local watcher picked up off a mounted share carries no HTTP
// transport at all, and its authentication is filesystem permissions on that
// share — the same authentication the retailer's own nightly job relies on.
// A drop uploaded over HTTP is a different proposition and must be signed.
func (*Adapter) Verify(_ context.Context, d *adapter.Delivery) error {
	if d.Method == "" {
		return nil
	}
	if accepted, configured := adapter.VerifyPeerIdentity(d.Binding, d.PeerIdentity); configured {
		if accepted {
			return nil
		}
		return adapter.Unauthorized("peer_not_allowed", "client certificate is not in the binding's allow-list")
	}
	return adapter.VerifyHMACSHA256(
		d.Binding.Secrets.HMACKey, d.Body, d.Header(HeaderSignature), adapter.EncodingHex, "")
}

// IdempotencyParts identifies a drop by its name and content digest.
//
// Both, not either. The name alone would suppress a corrected file re-uploaded
// under the same name, which is exactly how a retailer fixes a bad export. The
// digest alone would let the same content be processed twice under two names,
// which is what happens when someone copies yesterday's file to be safe.
func (*Adapter) IdempotencyParts(d *adapter.Delivery) []string {
	name := d.Header(HeaderFileName)
	digest := d.Header(HeaderFileSHA256)
	if name == "" && digest == "" {
		return nil
	}
	return []string{"file=" + name, "sha256=" + digest}
}

// Ingest parses a drop into one price change per usable row.
func (a *Adapter) Ingest(_ context.Context, d *adapter.Delivery) ([]canon.PriceChangeRequested, error) {
	opts, err := optionsOf(d)
	if err != nil {
		return nil, err
	}
	body := d.Body
	if opts.Encoding != "" {
		decoded, err := codepage.Decode(opts.Encoding, body)
		if err != nil {
			return nil, adapter.Malformed("bad_encoding",
				fmt.Sprintf("the file could not be decoded as %s: %v", opts.Encoding, err), err)
		}
		body = decoded
	}
	if t, err := time.Parse(time.RFC3339, d.Header(HeaderFileModTime)); err == nil {
		d.SourceTime = t.UTC()
	}

	var rows []row
	if opts.Format == FormatFixed {
		rows, err = readFixed(body, opts)
	} else {
		rows, err = readDelimited(body, opts)
	}
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, adapter.Malformed("empty_file", "the file contains no data rows", nil)
	}

	out := make([]canon.PriceChangeRequested, 0, len(rows))
	var failures []adapter.RowFailure
	for _, r := range rows {
		pc, ref, err := buildChange(opts, r)
		if err != nil {
			cls := adapter.Classify(err)
			failures = append(failures, adapter.RowFailure{
				Index:  r.index,
				Ref:    ref,
				Reason: cls.Reason,
				Detail: fmt.Sprintf("line %d: %s", r.line, cls.Detail),
			})
			continue
		}
		out = append(out, pc)
	}

	if len(failures) > 0 {
		ratio := float64(len(failures)) / float64(len(rows))
		if ratio > opts.MaxRowFailureRatio {
			// The file is structurally wrong, not merely imperfect. Publishing
			// the rows that happened to parse would put a mixture of correct
			// and misaligned prices on shelves, which is worse than publishing
			// nothing and quarantining the file for a human.
			return nil, adapter.Malformed("too_many_bad_rows",
				fmt.Sprintf("%d of %d rows were unusable (%.0f%%, limit %.0f%%); the file looks structurally wrong",
					len(failures), len(rows), ratio*100, opts.MaxRowFailureRatio*100), nil)
		}
		return out, &adapter.PartialError{Failures: failures, Total: len(rows)}
	}
	return out, nil
}

// row is one parsed record: either named cells (delimited with a header),
// positional cells (delimited without one), or the raw characters of a
// fixed-width line.
type row struct {
	// index is the zero-based position of the record among the file's data
	// rows, which is what an operator counts when they open the file.
	index int
	// line is the physical line number in the file, reported in the failure
	// detail because "row 437" and "line 439" are different numbers once a
	// header and a banner are involved.
	line   int
	cells  []string
	byName map[string]string
	runes  []rune
}

func readDelimited(body []byte, opts *Options) ([]row, error) {
	text := stripBOM(string(body))
	lines := splitLines(text)
	lines = dropSkippedAndTrailers(lines, opts)
	if len(lines) == 0 {
		return nil, nil
	}
	delim, _ := delimiterRune(opts.Delimiter)
	quote, _ := quoteRune(opts.Quote)

	r := csv.NewReader(strings.NewReader(strings.Join(lines, "\n")))
	r.Comma = delim
	// FieldsPerRecord is disabled rather than inferred: a ragged row must be a
	// row failure, not a whole-file failure, and the reader refusing the file
	// on line 3,000 would take the other 39,999 rows with it.
	r.FieldsPerRecord = -1
	// LazyQuotes tolerates the bare quote characters that appear in product
	// descriptions written by people who measure things in inches.
	r.LazyQuotes = true
	r.TrimLeadingSpace = false
	if quote == -1 {
		// encoding/csv has no "no quoting" mode on read; LazyQuotes with a
		// quote character no file contains is the closest equivalent and is why
		// "none" is documented as best-effort.
		r.LazyQuotes = true
	}
	records, err := r.ReadAll()
	if err != nil {
		return nil, adapter.Malformed("csv_parse", "the file is not parseable as delimited text: "+err.Error(), err)
	}
	if len(records) == 0 {
		return nil, nil
	}

	var header []string
	start := 0
	if hasHeader(records[0], opts) {
		header = records[0]
		start = 1
	}
	out := make([]row, 0, len(records)-start)
	for i := start; i < len(records); i++ {
		rec := records[i]
		if isBlank(rec) {
			continue
		}
		rw := row{index: len(out), line: i + 1 + opts.SkipLines, cells: rec}
		if header != nil {
			rw.byName = make(map[string]string, len(rec))
			for j, h := range header {
				if j < len(rec) {
					rw.byName[normaliseHeader(h)] = rec[j]
				}
			}
		}
		out = append(out, rw)
	}
	return out, nil
}

func readFixed(body []byte, opts *Options) ([]row, error) {
	text := stripBOM(string(body))
	lines := splitLines(text)
	lines = dropSkippedAndTrailers(lines, opts)
	out := make([]row, 0, len(lines))
	skipHeader := opts.Header == HeaderAlways
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if i == 0 && skipHeader {
			continue
		}
		out = append(out, row{index: len(out), line: i + 1 + opts.SkipLines, runes: []rune(l)})
	}
	return out, nil
}

func hasHeader(first []string, opts *Options) bool {
	switch opts.Header {
	case HeaderAlways:
		return true
	case HeaderNever:
		return false
	}
	// Auto: the first row is a header when it names at least one configured
	// column. Comparing against the configuration rather than guessing from
	// data types is what makes the detection stable for a file whose first
	// product happens to be called "PRICE".
	names := map[string]bool{}
	for _, c := range opts.Columns {
		if c.Name != "" {
			names[normaliseHeader(c.Name)] = true
		}
	}
	for _, c := range opts.Attributes {
		if c.Name != "" {
			names[normaliseHeader(c.Name)] = true
		}
	}
	if len(names) == 0 {
		return false
	}
	for _, cell := range first {
		if names[normaliseHeader(cell)] {
			return true
		}
	}
	return false
}

func normaliseHeader(s string) string {
	return strings.ToLower(strings.TrimSpace(strings.Trim(s, "\"' ")))
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.Split(s, "\n")
}

func dropSkippedAndTrailers(lines []string, opts *Options) []string {
	if opts.SkipLines > 0 {
		if opts.SkipLines >= len(lines) {
			return nil
		}
		lines = lines[opts.SkipLines:]
	}
	if len(opts.TrailerPrefixes) == 0 {
		return lines
	}
	out := lines[:0:0]
	for _, l := range lines {
		drop := false
		for _, p := range opts.TrailerPrefixes {
			if p != "" && strings.HasPrefix(l, p) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, l)
		}
	}
	return out
}

func isBlank(rec []string) bool {
	for _, c := range rec {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}

// utf8BOM is the byte order mark a Windows-side export tool prepends.
const utf8BOM = "\xef\xbb\xbf"

func stripBOM(s string) string { return strings.TrimPrefix(s, utf8BOM) }

// cell extracts a column's raw text from a row.
func (r *row) cell(c *Column) (string, bool) {
	switch {
	case c.Const != "":
		return c.Const, true
	case c.Offset != nil:
		start := *c.Offset
		if start >= len(r.runes) {
			return "", false
		}
		end := start + c.Length
		if end > len(r.runes) {
			end = len(r.runes)
		}
		return string(r.runes[start:end]), true
	case c.Index != nil:
		if *c.Index >= len(r.cells) {
			return "", false
		}
		return r.cells[*c.Index], true
	case c.Name != "" && r.byName != nil:
		v, ok := r.byName[normaliseHeader(c.Name)]
		return v, ok
	}
	return "", false
}

func value(r *row, name string, c *Column) (string, bool, error) {
	if c == nil {
		return "", false, nil
	}
	raw, found := r.cell(c)
	// Every field in a flat file is padded; trimming is unconditional because a
	// SKU of "ESP-1KG   " and one of "ESP-1KG" are the same product and no
	// mainframe export has ever meant otherwise.
	raw = strings.TrimSpace(raw)
	if !found || raw == "" {
		if c.Default != "" {
			raw = c.Default
		} else if c.Optional {
			return "", false, nil
		} else if !found {
			return "", false, adapter.Invalid("column_missing",
				fmt.Sprintf("column %q is not present in the row", name), nil)
		} else {
			return "", false, adapter.Invalid("column_empty",
				fmt.Sprintf("column %q is empty", name), nil)
		}
	}
	if c.Upper {
		raw = strings.ToUpper(raw)
	}
	if c.StripLeadingZeros {
		trimmed := strings.TrimLeft(raw, "0")
		if trimmed == "" {
			trimmed = "0"
		}
		raw = trimmed
	}
	if len(c.Map) > 0 {
		mapped, ok := c.Map[raw]
		if !ok {
			return "", false, adapter.Invalid("value_unmapped",
				fmt.Sprintf("column %q value %q is not in the mapping table", name, raw), nil)
		}
		raw = mapped
	}
	return raw, true, nil
}

// buildChange turns one row into a canonical change, also returning the row's
// own identifier so that a failure can be reported against something a support
// engineer can find in the source system without opening the retained body.
func buildChange(opts *Options, r row) (canon.PriceChangeRequested, string, error) {
	var pc canon.PriceChangeRequested
	pc.SourceSystem = Name

	sku, _, err := value(&r, FieldSKU, opts.Columns[FieldSKU])
	if err != nil {
		return pc, "", err
	}
	ref := sku
	pc.SKU = canon.SKU(sku)

	currency, _, err := value(&r, FieldCurrency, opts.Columns[FieldCurrency])
	if err != nil {
		return pc, ref, err
	}
	store, _, err := value(&r, FieldStore, opts.Columns[FieldStore])
	if err != nil {
		return pc, ref, err
	}
	pc.StoreID = canon.StoreID(store)

	price, ok, err := moneyValue(opts, &r, FieldPrice, currency)
	if err != nil {
		return pc, ref, err
	}
	if !ok {
		return pc, ref, adapter.Invalid("missing_price", "the row carries no price", nil)
	}
	pc.Price = price

	if was, ok, err := moneyValue(opts, &r, FieldWasPrice, currency); err != nil {
		return pc, ref, err
	} else if ok && was.Amount > price.Amount {
		pc.WasPrice = &was
	}
	if unit, ok, err := moneyValue(opts, &r, FieldUnitPrice, currency); err != nil {
		return pc, ref, err
	} else if ok {
		pc.UnitPrice = &unit
	}
	if v, _, err := value(&r, FieldUnitMeasure, opts.Columns[FieldUnitMeasure]); err != nil {
		return pc, ref, err
	} else {
		pc.UnitMeasure = v
	}
	if v, _, err := value(&r, FieldPromotionID, opts.Columns[FieldPromotionID]); err != nil {
		return pc, ref, err
	} else {
		pc.PromotionID = canon.PromotionID(v)
	}
	if v, _, err := value(&r, FieldReason, opts.Columns[FieldReason]); err != nil {
		return pc, ref, err
	} else {
		pc.Reason = v
	}
	if t, ok, err := timeValue(&r, FieldEffectiveAt, opts.Columns[FieldEffectiveAt]); err != nil {
		return pc, ref, err
	} else if ok {
		pc.EffectiveAt = t
	}
	if t, ok, err := timeValue(&r, FieldExpiresAt, opts.Columns[FieldExpiresAt]); err != nil {
		return pc, ref, err
	} else if ok {
		exp := t
		pc.ExpiresAt = &exp
	}
	for _, key := range opts.attrKey {
		v, ok, err := value(&r, key, opts.Attributes[key])
		if err != nil {
			return pc, ref, err
		}
		if !ok || v == "" {
			continue
		}
		if pc.Attributes == nil {
			pc.Attributes = map[string]string{}
		}
		pc.Attributes[key] = v
	}
	return pc, ref, nil
}

func moneyValue(opts *Options, r *row, name, currency string) (canon.Money, bool, error) {
	c := opts.Columns[name]
	if c == nil {
		return canon.Money{}, false, nil
	}
	raw, ok, err := value(r, name, c)
	if err != nil || !ok || raw == "" {
		return canon.Money{}, false, err
	}
	if c.Decimals != nil {
		m, err := decimal.ShiftedToMinor(raw, *c.Decimals, currency)
		if err != nil {
			return canon.Money{}, false, adapter.Invalid("price_unusable",
				fmt.Sprintf("column %q value %q with %d implied decimals in %s: %v",
					name, raw, *c.Decimals, currency, err), err)
		}
		return m, true, nil
	}
	m, err := decimal.ToMinorFormat(raw, currency, opts.format)
	if err != nil {
		return canon.Money{}, false, adapter.Invalid("price_unusable",
			fmt.Sprintf("column %q value %q in %s: %v", name, raw, currency, err), err)
	}
	return m, true, nil
}

func timeValue(r *row, name string, c *Column) (time.Time, bool, error) {
	if c == nil {
		return time.Time{}, false, nil
	}
	raw, ok, err := value(r, name, c)
	if err != nil || !ok || raw == "" {
		return time.Time{}, false, err
	}
	layout, err := resolveLayout(c.Layout)
	if err != nil {
		return time.Time{}, false, adapter.Invalid("bad_layout", err.Error(), err)
	}
	t, perr := time.ParseInLocation(layout, raw, time.UTC)
	if perr != nil {
		return time.Time{}, false, adapter.Invalid("date_unusable",
			fmt.Sprintf("column %q value %q does not match layout %q", name, raw, layout), perr)
	}
	return t.UTC(), true, nil
}

// Decimals is a helper for building a Column's implied-decimal count in
// configuration written in Go rather than JSON — the sample bindings the
// service ships with, and the test suite.
func Decimals(n int) *int { return &n }

// At builds a fixed-width Column position.
func At(offset, length int) (*int, int) { return &offset, length }
