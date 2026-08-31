package domain

import (
	"testing"
	"time"
)

// timeIn returns midday on the fifteenth of the given month, in UTC. Mid-month
// avoids any ambiguity about which season a boundary date belongs to.
func timeIn(month int) time.Time {
	return time.Date(2026, time.Month(month), 15, 12, 0, 0, 0, time.UTC)
}

func TestFeatureVectorOrderMatchesNames(t *testing.T) {
	f := Features{
		PriceMinor: 1, HourOfDay: 2, DayOfWeek: 3, DaysToExpiry: 4, Season: 5,
		InventoryLevel: 6, DaysOfSupply: 7, WasteRate: 8, CompetitorPrice: 9,
		Velocity7: 10, Velocity14: 11, Velocity30: 12, Elasticity: 13,
		WeatherBucket: 14, LocalEvent: true,
	}
	v := f.Vector()
	if len(v) != int(NumFeatures) {
		t.Fatalf("vector width %d, want %d", len(v), NumFeatures)
	}
	want := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 1}
	for i := range want {
		if v[i] != want[i] {
			t.Errorf("position %d (%s) = %v, want %v", i, FeatureNames[i], v[i], want[i])
		}
	}
	for i, n := range FeatureNames {
		if n == "" {
			t.Errorf("feature %d has no name", i)
		}
		idx, err := FeatureIndexByName(n)
		if err != nil || int(idx) != i {
			t.Errorf("FeatureIndexByName(%q) = %d, %v; want %d", n, idx, err, i)
		}
	}
}

func TestWithCalendar(t *testing.T) {
	local := time.Date(2026, 7, 4, 17, 30, 0, 0, time.UTC) // a Saturday
	f := Features{}.WithCalendar(local, false)
	if f.HourOfDay != 17 {
		t.Errorf("hour = %v, want 17", f.HourOfDay)
	}
	if f.DayOfWeek != 6 {
		t.Errorf("weekday = %v, want 6 (Saturday)", f.DayOfWeek)
	}
	if f.Season != 2 {
		t.Errorf("season = %v, want 2 (summer)", f.Season)
	}
}

func TestFillVectorPanicsOnShortBuffer(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("FillVector accepted a short buffer")
		}
	}()
	Features{}.FillVector(make([]float64, 2))
}
