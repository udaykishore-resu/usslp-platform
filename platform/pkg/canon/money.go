package canon

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Money is an exact monetary amount held in the smallest indivisible unit of
// its currency (cents for USD, paise for INR, yen for JPY).
//
// Floating point is never used for price anywhere in USSLP. A retailer whose
// displayed price differs from the charged price by one cent is in violation of
// weights-and-measures regulation in most of the platform's target markets, so
// the type system forbids the representation that would make it possible.
type Money struct {
	// Amount is the value in minor units. Negative amounts are legal and
	// represent credits or discounts.
	Amount int64 `json:"amount"`
	// Currency is the ISO 4217 alphabetic code, upper case.
	Currency string `json:"currency"`
}

// minorUnits maps ISO 4217 codes to their number of decimal places. Currencies
// absent from the table default to 2. The zero-decimal and three-decimal cases
// are the ones that break naive formatting.
var minorUnits = map[string]int{
	"JPY": 0, "KRW": 0, "VND": 0, "CLP": 0, "ISK": 0, "PYG": 0, "RWF": 0,
	"UGX": 0, "VUV": 0, "XAF": 0, "XOF": 0, "XPF": 0, "KMF": 0, "DJF": 0,
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3, "OMR": 3, "TND": 3,
}

// ErrCurrencyMismatch is returned when two amounts in different currencies are
// combined. It is never recoverable: the caller has a data modelling bug.
var ErrCurrencyMismatch = errors.New("canon: currency mismatch")

// NewMoney constructs an amount, normalising the currency code.
func NewMoney(amount int64, currency string) Money {
	return Money{Amount: amount, Currency: strings.ToUpper(strings.TrimSpace(currency))}
}

// Valid reports whether the currency code is a plausible ISO 4217 alphabetic
// code. It does not consult a registry; the UIG validates against the tenant's
// configured currency set at ingress.
func (m Money) Valid() bool {
	if len(m.Currency) != 3 {
		return false
	}
	for i := 0; i < 3; i++ {
		if m.Currency[i] < 'A' || m.Currency[i] > 'Z' {
			return false
		}
	}
	return true
}

// Exponent returns the number of decimal places for the amount's currency.
func (m Money) Exponent() int {
	if e, ok := minorUnits[m.Currency]; ok {
		return e
	}
	return 2
}

// Add returns the sum of two amounts of the same currency.
func (m Money) Add(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrCurrencyMismatch, m.Currency, o.Currency)
	}
	return Money{Amount: m.Amount + o.Amount, Currency: m.Currency}, nil
}

// Sub returns the difference of two amounts of the same currency.
func (m Money) Sub(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrCurrencyMismatch, m.Currency, o.Currency)
	}
	return Money{Amount: m.Amount - o.Amount, Currency: m.Currency}, nil
}

// PercentOff returns the amount reduced by pct percent, rounded half away from
// zero. Retail rounding must be deterministic and reproducible on the edge, so
// the rule is fixed here rather than left to each caller.
func (m Money) PercentOff(pct float64) Money {
	if pct <= 0 {
		return m
	}
	if pct >= 100 {
		return Money{Amount: 0, Currency: m.Currency}
	}
	// Work in integer basis points to keep the operation reproducible on a
	// 32-bit edge MCU that has no hardware FPU.
	bps := int64(pct*100 + 0.5) // percent -> basis points
	discount := roundDiv(m.Amount*bps, 10000)
	return Money{Amount: m.Amount - discount, Currency: m.Currency}
}

// roundDiv divides rounding half away from zero.
func roundDiv(num, den int64) int64 {
	if den == 0 {
		return 0
	}
	if (num < 0) != (den < 0) {
		return (num - den/2) / den
	}
	return (num + den/2) / den
}

// Cmp compares two amounts of the same currency: -1, 0 or +1.
func (m Money) Cmp(o Money) int {
	switch {
	case m.Amount < o.Amount:
		return -1
	case m.Amount > o.Amount:
		return 1
	default:
		return 0
	}
}

// String renders the amount for display and, more importantly, for the price
// attestation digest. Every tier must produce byte-identical output for the
// same amount or the attestation will not verify.
func (m Money) String() string {
	exp := m.Exponent()
	neg := m.Amount < 0
	v := m.Amount
	if neg {
		v = -v
	}
	if exp == 0 {
		if neg {
			return "-" + strconv.FormatInt(v, 10) + " " + m.Currency
		}
		return strconv.FormatInt(v, 10) + " " + m.Currency
	}
	div := int64(1)
	for i := 0; i < exp; i++ {
		div *= 10
	}
	whole := v / div
	frac := v % div
	var sb strings.Builder
	if neg {
		sb.WriteByte('-')
	}
	sb.WriteString(strconv.FormatInt(whole, 10))
	sb.WriteByte('.')
	fs := strconv.FormatInt(frac, 10)
	for i := len(fs); i < exp; i++ {
		sb.WriteByte('0')
	}
	sb.WriteString(fs)
	sb.WriteByte(' ')
	sb.WriteString(m.Currency)
	return sb.String()
}

// Display renders just the numeric portion with the currency symbol the label
// should show, e.g. "$2.49". Unknown currencies fall back to the ISO code.
func (m Money) Display() string {
	sym, ok := currencySymbols[m.Currency]
	num := strings.TrimSuffix(m.String(), " "+m.Currency)
	if !ok {
		return num + " " + m.Currency
	}
	if strings.HasPrefix(num, "-") {
		return "-" + sym + num[1:]
	}
	return sym + num
}

var currencySymbols = map[string]string{
	"USD": "$", "EUR": "€", "GBP": "£", "INR": "₹",
	"JPY": "¥", "CNY": "¥", "AUD": "A$", "CAD": "C$",
}

// MarshalJSON emits money as an object so that a consumer can never mistake the
// magnitude for a major-unit value.
func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Amount   int64  `json:"amount_minor"`
		Currency string `json:"currency"`
		Display  string `json:"display"`
	}{m.Amount, m.Currency, m.Display()})
}

// UnmarshalJSON accepts the canonical object form and, for POS adapters that
// emit a bare integer, a plain number in minor units with the currency supplied
// by the surrounding message.
func (m *Money) UnmarshalJSON(b []byte) error {
	var obj struct {
		Amount     *int64 `json:"amount_minor"`
		AmountAlt  *int64 `json:"amount"`
		PriceCents *int64 `json:"price_cents"`
		Currency   string `json:"currency"`
	}
	if err := json.Unmarshal(b, &obj); err == nil {
		switch {
		case obj.Amount != nil:
			m.Amount = *obj.Amount
		case obj.AmountAlt != nil:
			m.Amount = *obj.AmountAlt
		case obj.PriceCents != nil:
			m.Amount = *obj.PriceCents
		}
		m.Currency = strings.ToUpper(obj.Currency)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("canon: cannot decode money from %s", string(b))
	}
	m.Amount = n
	return nil
}
