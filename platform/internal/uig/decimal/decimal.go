// Package decimal converts the decimal representations that point-of-sale
// systems put on the wire into canon.Money, which is an exact integer count of
// minor units.
//
// It exists as its own package for one reason: every adapter needs this
// conversion, every adapter must do it identically, and none of them may use
// floating point to do it. A retailer whose shelf shows 2.48 because a POS sent
// the string "2.49" and something in the middle round-tripped it through a
// float64 is in breach of weights-and-measures regulation, and the defect is
// invisible until an inspector finds it. Keeping the conversion in one audited
// place, expressed entirely in integer and string arithmetic, is what makes
// that failure mode structurally impossible rather than merely unlikely.
//
// The three shapes the platform's adapters actually encounter are all handled
// here:
//
//   - decimal strings ("2.49", "1,234.56", "2,49" in a European locale, "249-"
//     with the trailing sign an AS/400 or SAP field carries) — ToMinor;
//   - an integer mantissa plus an explicit decimal-shift field, which is how
//     SAP IDoc conditions and several ERP file exports encode price —
//     ShiftedToMinor;
//   - an integer already counted in minor units, which is what Square and
//     Clover send — FromMinorUnits.
//
// Rounding, when a source carries more precision than the currency has minor
// units, is half away from zero. That is the rule canon.Money.PercentOff
// already uses, and matching it means a price that arrives as 0.005 and a price
// computed as a 50% discount off 0.01 land on the same cent.
package decimal

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// ErrSyntax means the text is not a decimal number in the expected format. It
// is always a permanent, client-side error: the same bytes will fail the same
// way forever, so a delivery carrying it must be quarantined and answered 4xx
// rather than retried.
var ErrSyntax = errors.New("uig/decimal: malformed decimal")

// ErrRange means the value does not fit in the int64 minor-unit representation.
// In practice this only happens on corrupt input — a genuine retail price never
// approaches 9.2e18 minor units — so it is treated exactly like a syntax error.
var ErrRange = errors.New("uig/decimal: value out of range")

// ErrCurrency means the currency code is not a plausible ISO 4217 alphabetic
// code. The exponent (and therefore the meaning of every digit) depends on it,
// so a missing or malformed code cannot be defaulted away silently.
var ErrCurrency = errors.New("uig/decimal: invalid currency")

// maxDigits caps how long a digit string may get after scaling. Nineteen digits
// is the width of int64; the extra headroom lets a legitimately padded source
// field ("000000000000249") through while still refusing a megabyte of digits
// that would otherwise be allocated and parsed before failing.
const maxDigits = 40

// Format describes the punctuation a source uses for decimals and thousands
// separators.
//
// This is a per-binding property rather than a global one because the same
// platform ingests a US chain sending "1,234.56" and a German chain sending
// "1.234,56" on the same afternoon, and guessing from the string is ambiguous
// exactly where it matters: "1.234" is one thousand two hundred and thirty-four
// euros in one convention and one euro twenty-three in the other.
type Format struct {
	// Decimal is the radix character, '.' or ','.
	Decimal rune
	// Group is the thousands separator, stripped before parsing. A zero value
	// means the source uses no grouping at all, in which case an unexpected
	// separator is an error rather than something to ignore.
	Group rune
}

// Plain is the format the overwhelming majority of POS APIs use: a dot radix
// and, in well-behaved sources, no grouping at all. Commas are tolerated as
// grouping because a surprising number of CSV exports include them.
var Plain = Format{Decimal: '.', Group: ','}

// European is the format used by continental European ERP exports.
var European = Format{Decimal: ',', Group: '.'}

func (f Format) withDefaults() Format {
	if f.Decimal == 0 {
		f.Decimal = '.'
	}
	return f
}

// Exponent returns the number of minor-unit digits for an ISO 4217 code: 2 for
// most currencies, 0 for JPY and its zero-decimal peers, 3 for the Gulf dinars.
// It delegates to canon so that the UIG and the label firmware can never
// disagree about how many digits a price has.
func Exponent(currency string) int {
	return canon.Money{Currency: normaliseCurrency(currency)}.Exponent()
}

func normaliseCurrency(c string) string {
	return strings.ToUpper(strings.TrimSpace(c))
}

func checkCurrency(c string) (string, error) {
	c = normaliseCurrency(c)
	if !(canon.Money{Currency: c}).Valid() {
		return "", fmt.Errorf("%w: %q", ErrCurrency, c)
	}
	return c, nil
}

// ToMinor converts a decimal string in the Plain format to exact minor units.
func ToMinor(s, currency string) (canon.Money, error) {
	return ToMinorFormat(s, currency, Plain)
}

// ToMinorFormat converts a decimal string to exact minor units using an
// explicit punctuation convention.
//
// It accepts a leading or trailing sign — the trailing form ("249-") is the
// overpunch-adjacent convention that SAP condition fields and AS/400 fixed-width
// exports still emit, and rejecting it would mean every such adapter
// re-implementing the same fix-up.
func ToMinorFormat(s, currency string, f Format) (canon.Money, error) {
	cur, err := checkCurrency(currency)
	if err != nil {
		return canon.Money{}, err
	}
	p, err := parse(s, f.withDefaults())
	if err != nil {
		return canon.Money{}, err
	}
	amount, err := p.toMinor(Exponent(cur))
	if err != nil {
		return canon.Money{}, err
	}
	return canon.Money{Amount: amount, Currency: cur}, nil
}

// ShiftedToMinor converts an integer mantissa plus an explicit decimal-shift
// count into exact minor units: mantissa 249 with shift 2 is 2.49.
//
// SAP IDoc price conditions carry the shift in its own field (and so do several
// ERP flat-file exports) because the condition value itself is a fixed-width
// integer with no radix point. The shift is *not* the currency's exponent: SAP
// routinely sends a shift of 4 for a two-decimal currency to carry sub-cent
// condition granularity, which is precisely why the conversion has to round
// rather than truncate.
//
// A negative shift multiplies instead of dividing, which is how "price per
// thousand" condition units are occasionally expressed.
func ShiftedToMinor(mantissa string, shift int, currency string) (canon.Money, error) {
	cur, err := checkCurrency(currency)
	if err != nil {
		return canon.Money{}, err
	}
	p, err := parse(mantissa, Format{Decimal: '.'})
	if err != nil {
		return canon.Money{}, err
	}
	// A mantissa that already carries a radix point *and* a shift field is
	// contradictory input; refusing it is safer than picking one and being
	// wrong about a price.
	if p.scale != 0 {
		return canon.Money{}, fmt.Errorf("%w: shifted mantissa %q must not contain a radix point", ErrSyntax, mantissa)
	}
	p.scale = shift
	amount, err := p.toMinor(Exponent(cur))
	if err != nil {
		return canon.Money{}, err
	}
	return canon.Money{Amount: amount, Currency: cur}, nil
}

// FromMinorUnits wraps an amount that is already counted in minor units, which
// is what Square's price_money and Clover's price field carry. It exists so
// that even the no-op conversion goes through one currency-validating path.
func FromMinorUnits(amount int64, currency string) (canon.Money, error) {
	cur, err := checkCurrency(currency)
	if err != nil {
		return canon.Money{}, err
	}
	return canon.Money{Amount: amount, Currency: cur}, nil
}

// parsed is a sign, a run of decimal digits, and the power of ten to divide by.
// Keeping the digits as text until the very last step is what makes the whole
// package float-free and overflow-detecting rather than overflow-silent.
type parsed struct {
	neg    bool
	digits string
	scale  int
}

func parse(s string, f Format) (parsed, error) {
	orig := s
	s = strings.TrimSpace(s)
	if s == "" {
		return parsed{}, fmt.Errorf("%w: empty", ErrSyntax)
	}
	neg := false
	// Leading sign.
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	// Trailing sign, as emitted by SAP condition fields and AS/400 exports.
	if n := len(s); n > 0 && (s[n-1] == '-' || s[n-1] == '+') {
		if neg {
			return parsed{}, fmt.Errorf("%w: %q has two signs", ErrSyntax, orig)
		}
		neg = s[n-1] == '-'
		s = s[:n-1]
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return parsed{}, fmt.Errorf("%w: %q has no digits", ErrSyntax, orig)
	}

	var intPart, fracPart strings.Builder
	seenRadix := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			if seenRadix {
				fracPart.WriteRune(r)
			} else {
				intPart.WriteRune(r)
			}
		case r == f.Decimal:
			if seenRadix {
				return parsed{}, fmt.Errorf("%w: %q has two radix points", ErrSyntax, orig)
			}
			seenRadix = true
		case f.Group != 0 && r == f.Group:
			// Grouping separators are positional decoration, not data. They are
			// only legal before the radix point; after it they are meaningless.
			if seenRadix {
				return parsed{}, fmt.Errorf("%w: %q groups digits after the radix point", ErrSyntax, orig)
			}
		default:
			return parsed{}, fmt.Errorf("%w: %q contains %q", ErrSyntax, orig, string(r))
		}
	}
	digits := intPart.String() + fracPart.String()
	if digits == "" {
		return parsed{}, fmt.Errorf("%w: %q has no digits", ErrSyntax, orig)
	}
	return parsed{neg: neg, digits: digits, scale: fracPart.Len()}, nil
}

// toMinor rescales the digits from scale decimal places to exp decimal places,
// rounding half away from zero, and returns the signed integer result.
func (p parsed) toMinor(exp int) (int64, error) {
	digits := p.digits
	scale := p.scale

	var n int64
	var roundUp bool
	switch {
	case exp >= scale:
		pad := exp - scale
		if len(digits)+pad > maxDigits {
			return 0, fmt.Errorf("%w: %s scaled by 10^%d", ErrRange, digits, pad)
		}
		digits += strings.Repeat("0", pad)
	default:
		drop := scale - exp
		if len(digits) > maxDigits {
			return 0, fmt.Errorf("%w: %d digits", ErrRange, len(digits))
		}
		// Left-pad so there is always at least one kept digit and exactly one
		// well-defined first-dropped digit, even for "0.005" at exponent 2.
		if len(digits) <= drop {
			digits = strings.Repeat("0", drop+1-len(digits)) + digits
		}
		keep := len(digits) - drop
		roundUp = digits[keep] >= '5'
		digits = digits[:keep]
	}
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s", ErrRange, digits)
	}
	n = v
	if roundUp {
		if n == 1<<63-1 {
			return 0, fmt.Errorf("%w: rounding overflows int64", ErrRange)
		}
		n++
	}
	if p.neg {
		n = -n
	}
	return n, nil
}
