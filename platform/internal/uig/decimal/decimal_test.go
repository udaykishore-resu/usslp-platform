package decimal

import (
	"errors"
	"strings"
	"testing"
)

func TestToMinorBoundaries(t *testing.T) {
	cases := []struct {
		in       string
		currency string
		want     int64
	}{
		// The ordinary case every webhook sends.
		{"2.49", "USD", 249},
		{"0.01", "USD", 1},
		{"0.00", "USD", 0},
		{"10", "USD", 1000},
		{".5", "USD", 50},
		{"1234.56", "USD", 123456},
		{"1,234.56", "USD", 123456},
		{"-3.75", "USD", -375},
		// Half away from zero, in both directions.
		{"0.005", "USD", 1},
		{"-0.005", "USD", -1},
		{"0.004", "USD", 0},
		{"0.015", "USD", 2},
		{"9.999", "USD", 1000},
		// Zero-decimal currency: the digits after the point are precision the
		// currency does not have.
		{"249", "JPY", 249},
		{"249.4", "JPY", 249},
		{"249.5", "JPY", 250},
		{"-249.5", "JPY", -250},
		// Three-decimal currency: the same string means a thousand times less.
		{"2.49", "KWD", 2490},
		{"2.499", "KWD", 2499},
		{"2.4995", "KWD", 2500},
		{"0.0005", "KWD", 1},
		{"0.0004", "KWD", 0},
		// Trailing sign, as SAP and AS/400 emit it.
		{"249-", "JPY", -249},
		{"2.49-", "USD", -249},
		{"2.49+", "USD", 249},
	}
	for _, c := range cases {
		got, err := ToMinor(c.in, c.currency)
		if err != nil {
			t.Fatalf("ToMinor(%q, %s): %v", c.in, c.currency, err)
		}
		if got.Amount != c.want {
			t.Errorf("ToMinor(%q, %s) = %d, want %d", c.in, c.currency, got.Amount, c.want)
		}
		if got.Currency != c.currency {
			t.Errorf("ToMinor(%q, %s) currency = %q", c.in, c.currency, got.Currency)
		}
	}
}

func TestToMinorRoundTripsThroughCanonString(t *testing.T) {
	// The attestation digest is computed over canon.Money.String(), so a value
	// that survives ingress must render back to the text a retailer typed.
	for _, c := range []struct{ in, currency, want string }{
		{"2.49", "USD", "2.49 USD"},
		{"2.5", "USD", "2.50 USD"},
		{"249", "JPY", "249 JPY"},
		{"2.499", "KWD", "2.499 KWD"},
	} {
		m, err := ToMinor(c.in, c.currency)
		if err != nil {
			t.Fatalf("ToMinor(%q): %v", c.in, err)
		}
		if got := m.String(); got != c.want {
			t.Errorf("ToMinor(%q,%s).String() = %q, want %q", c.in, c.currency, got, c.want)
		}
	}
}

func TestEuropeanFormat(t *testing.T) {
	m, err := ToMinorFormat("1.234,56", "EUR", European)
	if err != nil {
		t.Fatalf("European: %v", err)
	}
	if m.Amount != 123456 {
		t.Fatalf("European 1.234,56 = %d, want 123456", m.Amount)
	}
	// The same text under the plain convention is a completely different price;
	// that is the whole reason the convention is configured per binding.
	m2, err := ToMinorFormat("1.234", "EUR", Plain)
	if err != nil {
		t.Fatalf("plain: %v", err)
	}
	if m2.Amount != 123 {
		t.Fatalf("plain 1.234 = %d, want 123", m2.Amount)
	}
}

func TestToMinorRejectsMalformed(t *testing.T) {
	bad := []string{"", "   ", "abc", "2.4.9", "2..49", "$2.49", "2 49", "-", "+-2", "--2", "1.23,45", "2.49-3"}
	for _, s := range bad {
		if _, err := ToMinor(s, "USD"); !errors.Is(err, ErrSyntax) {
			t.Errorf("ToMinor(%q) err = %v, want ErrSyntax", s, err)
		}
	}
	if _, err := ToMinor("2.49", "US"); !errors.Is(err, ErrCurrency) {
		t.Errorf("bad currency err = %v, want ErrCurrency", err)
	}
	if _, err := ToMinor("2.49", "usd"); err != nil {
		t.Errorf("lower-case currency should normalise, got %v", err)
	}
	if _, err := ToMinor(strings.Repeat("9", 60), "USD"); !errors.Is(err, ErrRange) {
		t.Errorf("60-digit value err = %v, want ErrRange", err)
	}
}

func TestShiftedToMinor(t *testing.T) {
	cases := []struct {
		mantissa string
		shift    int
		currency string
		want     int64
	}{
		// The plain SAP case: a fixed-width condition value plus its shift.
		{"0000249", 2, "EUR", 249},
		{"249", 2, "EUR", 249},
		{"24900", 4, "EUR", 249},
		// Shift finer than the currency: SAP carries sub-cent condition
		// granularity and the conversion must round, not truncate.
		{"24950", 4, "EUR", 250},
		{"24949", 4, "EUR", 249},
		{"5", 3, "USD", 1},
		{"4", 3, "USD", 0},
		// Shift coarser than the currency.
		{"249", 0, "EUR", 24900},
		{"249", 1, "EUR", 2490},
		// Zero- and three-decimal currencies.
		{"24900", 2, "JPY", 249},
		{"249", 0, "KWD", 249000},
		{"2499", 3, "KWD", 2499},
		// Negative shift multiplies: "price per thousand" condition units.
		{"249", -1, "EUR", 249000},
		// Trailing SAP sign.
		{"0000249-", 2, "EUR", -249},
	}
	for _, c := range cases {
		got, err := ShiftedToMinor(c.mantissa, c.shift, c.currency)
		if err != nil {
			t.Fatalf("ShiftedToMinor(%q,%d,%s): %v", c.mantissa, c.shift, c.currency, err)
		}
		if got.Amount != c.want {
			t.Errorf("ShiftedToMinor(%q,%d,%s) = %d, want %d",
				c.mantissa, c.shift, c.currency, got.Amount, c.want)
		}
	}
	if _, err := ShiftedToMinor("2.49", 2, "EUR"); !errors.Is(err, ErrSyntax) {
		t.Error("a mantissa with a radix point plus a shift field is contradictory and must be refused")
	}
}

func TestFromMinorUnits(t *testing.T) {
	m, err := FromMinorUnits(249, "usd")
	if err != nil {
		t.Fatalf("FromMinorUnits: %v", err)
	}
	if m.Amount != 249 || m.Currency != "USD" {
		t.Fatalf("FromMinorUnits = %+v", m)
	}
	if _, err := FromMinorUnits(249, "US$"); !errors.Is(err, ErrCurrency) {
		t.Errorf("err = %v, want ErrCurrency", err)
	}
}

func TestExponent(t *testing.T) {
	for cur, want := range map[string]int{"USD": 2, "EUR": 2, "JPY": 0, "KRW": 0, "KWD": 3, "BHD": 3, "ZZZ": 2} {
		if got := Exponent(cur); got != want {
			t.Errorf("Exponent(%s) = %d, want %d", cur, got, want)
		}
	}
}
