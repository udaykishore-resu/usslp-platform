package app

import (
	"math"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/pkg/canon"
)

// syntheticTelemetry generates a healthy fleet in the service's own feature
// shape. All of the data in this file is synthetic.
func syntheticTelemetry(n int, seed uint64) []TelemetryFeatures {
	r := seed
	next := func() float64 {
		r += 0x9E3779B97F4A7C15
		z := r
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		return float64(z>>11) / float64(1<<53)
	}
	normal := func() float64 {
		u1 := next()
		if u1 < 1e-12 {
			u1 = 1e-12
		}
		return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*next())
	}
	out := make([]TelemetryFeatures, n)
	for i := range out {
		temp := 20 + 2*normal()
		if next() < 0.2 {
			temp = 4 + 1.5*normal()
		}
		out[i] = TelemetryFeatures{
			BatteryMV:           3000 + 60*normal(),
			TemperatureC:        temp,
			RefreshPerDay:       math.Exp(1.2 + 0.4*normal()),
			LQI:                 200 - math.Abs(30*normal()),
			BatteryDropMVPerDay: 0.4 + 0.15*math.Abs(normal()),
			RSSI:                -60 + 8*normal(),
			HasHistory:          true,
		}
	}
	return out
}

// TestDetectorMeasuresItsOwnErrorRates trains the detector on a synthetic fleet
// and reports both rates, which is the only honest way to characterise it.
func TestDetectorMeasuresItsOwnErrorRates(t *testing.T) {
	train := syntheticTelemetry(2000, 8080)
	det, err := NewAnomalyDetector(train, DefaultContamination, 512)
	if err != nil {
		t.Fatalf("train: %v", err)
	}

	healthy := syntheticTelemetry(2000, 424242)
	faulty := []TelemetryFeatures{
		{BatteryMV: 2000, TemperatureC: 20, RefreshPerDay: 3.3, LQI: 190, BatteryDropMVPerDay: 0.4, RSSI: -60, HasHistory: true},
		{BatteryMV: 3000, TemperatureC: 65, RefreshPerDay: 3.3, LQI: 190, BatteryDropMVPerDay: 0.4, RSSI: -60, HasHistory: true},
		{BatteryMV: 3000, TemperatureC: 20, RefreshPerDay: 600, LQI: 190, BatteryDropMVPerDay: 0.4, RSSI: -60, HasHistory: true},
		{BatteryMV: 3000, TemperatureC: 20, RefreshPerDay: 3.3, LQI: 8, BatteryDropMVPerDay: 0.4, RSSI: -60, HasHistory: true},
		{BatteryMV: 2950, TemperatureC: 20, RefreshPerDay: 3.3, LQI: 190, BatteryDropMVPerDay: 40, RSSI: -60, HasHistory: true},
		{BatteryMV: 3000, TemperatureC: 20, RefreshPerDay: 3.3, LQI: 190, BatteryDropMVPerDay: 0.4, RSSI: -115, HasHistory: true},
	}

	fpr := det.FalsePositiveRate(healthy)
	dr := det.DetectionRate(faulty)
	t.Logf("synthetic fleet of %d labels: threshold %.4f at %.1f%% contamination; "+
		"measured false-positive rate %.2f%% on %d fresh healthy labels, "+
		"detection %.0f%% on %d injected faults",
		len(train), det.Threshold(), 100*DefaultContamination, 100*fpr, len(healthy), 100*dr, len(faulty))

	if dr < 1 {
		t.Errorf("detection rate %.0f%%: the injected faults are far outside the fleet and should all be caught", 100*dr)
	}
	if fpr > 0.06 {
		t.Errorf("false-positive rate %.2f%% is far above the %.1f%% the threshold encodes",
			100*fpr, 100*DefaultContamination)
	}
}

func TestDetectorRecordsAndRanksFlags(t *testing.T) {
	det, err := NewAnomalyDetector(syntheticTelemetry(1500, 1234), DefaultContamination, 64)
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	now := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		label canon.LabelID
		feats TelemetryFeatures
	}{
		{"lbl-mild", TelemetryFeatures{BatteryMV: 2700, TemperatureC: 20, RefreshPerDay: 3.3, LQI: 190, RSSI: -60}},
		{"lbl-dead", TelemetryFeatures{BatteryMV: 900, TemperatureC: 20, RefreshPerDay: 3.3, LQI: 190, RSSI: -60}},
		// A brand-new label with no discharge history at all: the detector must
		// impute rather than treat the unknown rate as an observed zero.
		{"lbl-ok", TelemetryFeatures{BatteryMV: 3000, TemperatureC: 20, RefreshPerDay: 3.3, LQI: 190, RSSI: -60}},
	}
	for _, c := range cases {
		det.Evaluate(canon.Telemetry{LabelID: c.label, StoreID: "s1", ReportedAt: now}, "acme", c.feats)
	}
	recent := det.Recent("", 10)
	if len(recent) == 0 {
		t.Fatal("no flags recorded")
	}
	if recent[0].LabelID != "lbl-dead" {
		t.Errorf("highest-scoring flag is %s, want lbl-dead", recent[0].LabelID)
	}
	for i := 1; i < len(recent); i++ {
		if recent[i-1].Score < recent[i].Score {
			t.Errorf("flags are not ranked by score at %d", i)
		}
	}
	for _, r := range recent {
		if r.Reason == "" || r.Feature == "" {
			t.Errorf("flag %s has no reason or feature: %+v", r.LabelID, r)
		}
	}
	// The healthy label must not appear.
	for _, r := range recent {
		if r.LabelID == "lbl-ok" {
			t.Errorf("a healthy label was flagged: %+v", r)
		}
	}

	if got := det.Recent("other-store", 10); len(got) != 0 {
		t.Errorf("store filter returned %d flags for a store with none", len(got))
	}
}

func TestDetectorRingWrapsWithoutLosingOrdering(t *testing.T) {
	det, err := NewAnomalyDetector(syntheticTelemetry(1200, 99), DefaultContamination, 4)
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	for i := 0; i < 20; i++ {
		det.Evaluate(canon.Telemetry{LabelID: canon.LabelID("lbl"), StoreID: "s1"}, "acme",
			TelemetryFeatures{BatteryMV: 500, TemperatureC: 20, RefreshPerDay: 3.3, LQI: 190, RSSI: -60})
	}
	if got := len(det.Recent("", 100)); got != 4 {
		t.Errorf("ring holds %d flags, want its capacity of 4", got)
	}
}
