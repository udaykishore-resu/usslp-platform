package app

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/usslp/usslp/platform/internal/pricing/ml"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// TelemetryFeatures is the isolation forest's view of one label report.
//
// Four raw signals, plus two derived rates. The derived ones matter more than
// the raw ones: a refresh count of 42,000 is meaningless without knowing the
// label has been in service for six years, whereas 300 refreshes per day is
// immediately wrong. Computing them here rather than expecting the caller to
// keeps the model's input definition in one place.
type TelemetryFeatures struct {
	BatteryMV     float64
	TemperatureC  float64
	RefreshPerDay float64
	LQI           float64
	// BatteryDropMVPerDay is the discharge rate implied by consecutive reports.
	// It is the earliest signal of a failing cell, months before the voltage
	// itself looks alarming.
	BatteryDropMVPerDay float64
	// RSSI completes the radio picture: a low LQI with a strong RSSI is
	// interference, a low LQI with a weak RSSI is distance.
	RSSI float64
	// HasHistory is false when this is the label's first report and the
	// discharge rate could not be computed.
	//
	// It exists because "we do not know the discharge rate" and "the discharge
	// rate is zero" are different facts with opposite meanings, and a zero fed
	// to the detector as though it were observed would flag every label in a
	// store rollout on its first heartbeat. The detector substitutes the fleet
	// median for the unknown signal, which is the one imputation that adds no
	// evidence in either direction.
	HasHistory bool
}

// TelemetryFeatureNames label the anomaly reasons.
var TelemetryFeatureNames = []string{
	"battery_mv", "temperature_c", "refreshes_per_day", "lqi", "battery_drop_mv_per_day", "rssi",
}

// Vector renders the features in model order.
func (t TelemetryFeatures) Vector() []float64 {
	return []float64{t.BatteryMV, t.TemperatureC, t.RefreshPerDay, t.LQI, t.BatteryDropMVPerDay, t.RSSI}
}

// featDischargeRate is the position of the discharge rate in the vector, the
// one signal that can legitimately be unknown.
const featDischargeRate = 4

// AnomalyRecord is one flagged label.
type AnomalyRecord struct {
	LabelID canon.LabelID  `json:"label_id"`
	StoreID canon.StoreID  `json:"store_id"`
	Tenant  canon.TenantID `json:"tenant_id"`
	// Score is the isolation score in [0, 1].
	Score float64 `json:"score"`
	// Reason is the operator-facing explanation.
	Reason string `json:"reason"`
	// Feature names the signal that drove the flag.
	Feature string `json:"feature"`
	// Gate says which mechanism raised the flag: "isolation" for the forest's
	// score, "envelope" for a signal outside anything the fleet reports, or
	// "both". An operator triaging a list treats them differently — an envelope
	// flag is a fact, an isolation flag is a judgement — so the record carries
	// the distinction rather than making them look alike.
	Gate string `json:"gate,omitempty"`
	// Observed is the telemetry the flag was raised on.
	Observed TelemetryFeatures `json:"-"`
	// DetectedAt is when the flag was raised.
	DetectedAt time.Time `json:"detected_at"`
	// ReportedAt is when the label produced the telemetry.
	ReportedAt time.Time `json:"reported_at"`
}

// AnomalyDetector scores label telemetry against a per-tenant isolation forest,
// with an envelope gate alongside it.
//
// # Why there are two mechanisms
//
// The isolation forest answers "is this label unlike the others", which is the
// question that finds the failures nobody wrote a rule for. It has one
// structural weakness: it chooses its split dimension at random, so a label that
// is catastrophically wrong in exactly one of six signals spends most of its
// path being split on dimensions where it looks ordinary. On this package's
// synthetic fleet the forest alone catches only about a third of injected
// single-signal faults at a 2% false-positive budget — while ranking them
// correctly, which is why its AUC stays high and its recall does not.
//
// The envelope gate closes that hole with a statement of fact rather than a
// statistical judgement: a signal outside anything the fleet has ever reported,
// by half the fleet's own observed spread again, is flagged whatever the
// multivariate score says. It is not a hand-tuned threshold — every bound is
// derived from the training population, so a chest-freezer fleet gets a
// freezer's envelope — and it cannot fire on a value the fleet routinely
// produces, because the envelope is built from exactly those values.
//
// Together: the forest ranks, and the gate guarantees the blatant cases are
// never missed. Both report through the same record, and Record.Gate says which
// mechanism fired.
//
// # Threshold selection
//
// The threshold is not a constant. An isolation score is only meaningful
// relative to the population the forest was trained on, and a fleet of new
// labels in a new store has a different score distribution from a six-year-old
// estate. The detector therefore sets its threshold at a quantile of the
// training population's own scores — "flag the most unusual two per cent of
// this fleet" — which keeps the alert volume predictable no matter what the
// fleet looks like. The alternative, a fixed 0.6, produces zero alerts in one
// store and forty thousand in another.
type AnomalyDetector struct {
	mu        sync.RWMutex
	forest    *ml.IsolationForest
	threshold float64
	// contamination is the fraction of the training population expected to be
	// anomalous, which sets the threshold quantile.
	contamination float64
	// envelopeLo and envelopeHi bound each signal at the range the training
	// fleet actually produced, widened by EnvelopeMargin of that range.
	envelopeLo []float64
	envelopeHi []float64
	// recent is a bounded ring of the most recent flags, which is what
	// GET /v1/anomalies serves. It is a cache in front of the analytics
	// service's durable history, not the history itself: this service must
	// answer "what is wrong right now" without a cross-service query.
	recent    []AnomalyRecord
	recentCap int
	next      int
	filled    bool
}

// EnvelopeMargin is how far outside the fleet's observed range a signal must go
// before the gate fires, as a fraction of that range.
//
// Half again. The margin exists because the observed range of a finite sample
// understates the population's range — the next thousand labels will produce a
// slightly colder freezer and a slightly weaker link than the training sample
// did — and firing on every such label would make the gate a noise generator.
// Half the observed spread is comfortably outside sampling variation and
// comfortably inside every fault mode the platform has a name for.
const EnvelopeMargin = 0.5

// EnvelopeQuantile is the quantile the envelope is built from, rather than the
// raw minimum and maximum.
//
// A single mis-decoded telemetry frame in the training sample — a battery
// reported as 65535 mV — would otherwise widen the envelope past every real
// fault and silently disable the gate. Trimming a thousandth from each tail
// costs nothing and removes that failure mode.
const EnvelopeQuantile = 0.001

// DefaultContamination is the fraction of a fleet the detector expects to be
// anomalous, and therefore where the score threshold is set.
//
// Two per cent, chosen from the measured detection curve in the ml package's
// tests rather than from a round number: on that synthetic fleet a 2%
// false-positive budget catches 97% of injected single-signal faults, 1% catches
// 84%, and 0.5% catches 72%. Two per cent of a 40,000-label store is 800 labels,
// which is far more than a store team works through in a day — but the flag is
// not the deliverable, the *ranking* is, and the top twenty by score are what a
// morning maintenance sweep actually picks up. Tightening the budget to reduce
// the list would drop real faults off the bottom of it.
//
// It is configurable because a newly commissioned store legitimately runs a
// higher anomaly rate for its first fortnight.
const DefaultContamination = 0.02

// NewAnomalyDetector trains a detector from a fleet telemetry sample.
func NewAnomalyDetector(sample []TelemetryFeatures, contamination float64, recentCap int) (*AnomalyDetector, error) {
	if contamination <= 0 || contamination >= 0.5 {
		contamination = DefaultContamination
	}
	if recentCap <= 0 {
		recentCap = 1024
	}
	rows := make([][]float64, len(sample))
	for i, s := range sample {
		rows[i] = s.Vector()
	}
	forest, err := ml.TrainIsolationForest(rows, ml.IsoForestParams{
		FeatureNames: TelemetryFeatureNames,
	})
	if err != nil {
		return nil, err
	}
	scores := make([]float64, len(rows))
	for i, r := range rows {
		scores[i] = forest.AnomalyScore(r)
	}
	d := &AnomalyDetector{
		forest:        forest,
		threshold:     ml.Quantile(scores, 1-contamination),
		contamination: contamination,
		recent:        make([]AnomalyRecord, recentCap),
		recentCap:     recentCap,
	}
	d.envelopeLo, d.envelopeHi = buildEnvelope(rows)
	// A degenerate training population — every label identical, or a sample too
	// small to have a tail — can put the quantile at or below the score's
	// mid-point, which would flag half the fleet. Floor it there: a detector
	// that cannot separate anything should flag nothing rather than everything.
	if d.threshold < 0.5 {
		d.threshold = 0.5
	}
	return d, nil
}

// NewAnomalyDetectorFromForest wraps an already-trained forest, which is how
// the service loads the registry's champion after a restart.
func NewAnomalyDetectorFromForest(forest *ml.IsolationForest, threshold float64, recentCap int) (*AnomalyDetector, error) {
	if forest == nil {
		return nil, fmt.Errorf("pricing: nil isolation forest")
	}
	if threshold <= 0 || threshold >= 1 {
		threshold = 0.6
	}
	if recentCap <= 0 {
		recentCap = 1024
	}
	// A detector rebuilt from a serialised forest has no training sample to
	// derive an envelope from, so the gate is reconstructed from the forest's
	// own robust statistics: median plus or minus six robust deviations, which
	// on a normal population is the same order as the 0.1st and 99.9th
	// percentiles the sample-based envelope uses.
	lo := make([]float64, len(forest.Median))
	hi := make([]float64, len(forest.Median))
	for i := range forest.Median {
		spread := 1.4826 * forest.MAD[i] * 6
		if spread <= 0 {
			spread = 1
		}
		lo[i] = forest.Median[i] - spread*(1+EnvelopeMargin)
		hi[i] = forest.Median[i] + spread*(1+EnvelopeMargin)
	}
	return &AnomalyDetector{
		forest: forest, threshold: threshold,
		contamination: DefaultContamination,
		envelopeLo:    lo, envelopeHi: hi,
		recent: make([]AnomalyRecord, recentCap), recentCap: recentCap,
	}, nil
}

// buildEnvelope derives the per-signal bounds from a training sample.
func buildEnvelope(rows [][]float64) (lo, hi []float64) {
	if len(rows) == 0 {
		return nil, nil
	}
	nf := len(rows[0])
	lo = make([]float64, nf)
	hi = make([]float64, nf)
	col := make([]float64, len(rows))
	for f := 0; f < nf; f++ {
		for i := range rows {
			col[i] = rows[i][f]
		}
		low := ml.Quantile(col, EnvelopeQuantile)
		high := ml.Quantile(col, 1-EnvelopeQuantile)
		span := high - low
		if span <= 0 {
			// A signal that never varies. One unit of margin keeps the gate
			// meaningful without making it fire on the fleet's own value.
			span = 1
		}
		lo[f] = low - EnvelopeMargin*span
		hi[f] = high + EnvelopeMargin*span
	}
	return lo, hi
}

// outsideEnvelope reports the first signal outside its bound, or -1.
func (d *AnomalyDetector) outsideEnvelope(v []float64) int {
	if len(d.envelopeLo) == 0 {
		return -1
	}
	worst, worstIdx := 0.0, -1
	for i := range v {
		if i >= len(d.envelopeLo) {
			break
		}
		var excess float64
		switch {
		case v[i] < d.envelopeLo[i]:
			excess = d.envelopeLo[i] - v[i]
		case v[i] > d.envelopeHi[i]:
			excess = v[i] - d.envelopeHi[i]
		default:
			continue
		}
		// Normalise the overshoot by the signal's own span so that "40 mV/day
		// past the envelope" and "40 dBm past it" are comparable, and the
		// reported signal is the one furthest out rather than the first found.
		span := d.envelopeHi[i] - d.envelopeLo[i]
		if span > 0 {
			excess /= span
		}
		if excess > worst {
			worst, worstIdx = excess, i
		}
	}
	return worstIdx
}

// Threshold reports the score at which a label is flagged.
func (d *AnomalyDetector) Threshold() float64 {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.threshold
}

// Forest exposes the underlying model so the service can register it.
func (d *AnomalyDetector) Forest() *ml.IsolationForest {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.forest
}

// Evaluate scores one telemetry report and records a flag if it crosses the
// threshold. It returns the score and the flag regardless, so a caller that
// wants every score (the analytics ingest does) gets it without a second API.
func (d *AnomalyDetector) Evaluate(t canon.Telemetry, tenant canon.TenantID, feats TelemetryFeatures) AnomalyRecord {
	d.mu.RLock()
	forest, threshold := d.forest, d.threshold
	v := feats.Vector()
	if !feats.HasHistory && featDischargeRate < len(forest.Median) {
		v[featDischargeRate] = forest.Median[featDischargeRate]
	}
	breach := d.outsideEnvelope(v)
	d.mu.RUnlock()

	a := forest.Evaluate(v, threshold)
	rec := AnomalyRecord{
		LabelID: t.LabelID, StoreID: t.StoreID, Tenant: tenant,
		Score: a.Score, Reason: a.Reason,
		Observed: feats, DetectedAt: time.Now().UTC(), ReportedAt: t.ReportedAt,
	}
	if a.TopFeature >= 0 && a.TopFeature < len(TelemetryFeatureNames) {
		rec.Feature = TelemetryFeatureNames[a.TopFeature]
	}
	switch {
	case a.Flagged && breach >= 0:
		rec.Gate = "both"
	case breach >= 0:
		rec.Gate = "envelope"
	case a.Flagged:
		rec.Gate = "isolation"
	}
	if breach >= 0 && breach < len(TelemetryFeatureNames) {
		// The envelope breach is the more concrete finding, so it wins the
		// headline. The isolation reason is still computed and still ranks the
		// record; what changes is what the operator reads first.
		rec.Feature = TelemetryFeatureNames[breach]
		rec.Reason = fmt.Sprintf("%s is %.3g, outside the [%.3g, %.3g] envelope this fleet has ever reported",
			TelemetryFeatureNames[breach], v[breach], d.envelopeLo[breach], d.envelopeHi[breach])
	}
	if rec.Gate == "" {
		return rec
	}
	d.mu.Lock()
	d.recent[d.next] = rec
	d.next = (d.next + 1) % d.recentCap
	if d.next == 0 {
		d.filled = true
	}
	d.mu.Unlock()
	return rec
}

// Recent returns the most recently flagged labels, highest score first,
// optionally filtered by store.
func (d *AnomalyDetector) Recent(store canon.StoreID, limit int) []AnomalyRecord {
	d.mu.RLock()
	defer d.mu.RUnlock()
	n := d.next
	if d.filled {
		n = d.recentCap
	}
	out := make([]AnomalyRecord, 0, n)
	for i := 0; i < n; i++ {
		r := d.recent[i]
		if r.LabelID == "" {
			continue
		}
		if store != "" && r.StoreID != store {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Envelope returns the per-signal bounds the gate enforces, for the API's
// diagnostics view.
func (d *AnomalyDetector) Envelope() (lo, hi []float64) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return append([]float64(nil), d.envelopeLo...), append([]float64(nil), d.envelopeHi...)
}

// FalsePositiveRate measures the detector against a labelled sample.
//
// It exists so that the alert volume a store will actually see is a measured
// number in a test and in the service's own metrics, not a hope. "Normal" here
// means the caller asserts these rows are healthy; on synthetic data that is
// exact, and on real fleet data it is a human judgement that the function
// reports rather than makes.
func (d *AnomalyDetector) FalsePositiveRate(normal []TelemetryFeatures) float64 {
	if len(normal) == 0 {
		return 0
	}
	d.mu.RLock()
	forest, threshold := d.forest, d.threshold
	d.mu.RUnlock()
	flagged := 0
	for _, f := range normal {
		v := f.Vector()
		if forest.AnomalyScore(v) >= threshold || d.outsideEnvelope(v) >= 0 {
			flagged++
		}
	}
	return float64(flagged) / float64(len(normal))
}

// DetectionRate measures the fraction of known anomalies the detector catches.
func (d *AnomalyDetector) DetectionRate(anomalous []TelemetryFeatures) float64 {
	if len(anomalous) == 0 {
		return 0
	}
	d.mu.RLock()
	forest, threshold := d.forest, d.threshold
	d.mu.RUnlock()
	caught := 0
	for _, f := range anomalous {
		v := f.Vector()
		if forest.AnomalyScore(v) >= threshold || d.outsideEnvelope(v) >= 0 {
			caught++
		}
	}
	return float64(caught) / float64(len(anomalous))
}

// TelemetryFeaturesFrom derives model features from a raw telemetry report and
// the label's previous report.
//
// The previous report may be zero, in which case the discharge rate is reported
// as zero rather than invented — a first report from a newly commissioned label
// genuinely carries no rate information, and a fabricated one would flag every
// new label in a store rollout.
func TelemetryFeaturesFrom(cur canon.Telemetry, prev canon.Telemetry) TelemetryFeatures {
	f := TelemetryFeatures{
		BatteryMV:    float64(cur.BatteryMV),
		TemperatureC: cur.TemperatureC,
		LQI:          float64(cur.LQI),
		RSSI:         float64(cur.RSSI),
	}
	if cur.UptimeSeconds > 0 {
		days := float64(cur.UptimeSeconds) / 86400
		if days >= 1 {
			f.RefreshPerDay = float64(cur.RefreshCount) / days
		} else {
			f.RefreshPerDay = float64(cur.RefreshCount)
		}
	}
	if !prev.ReportedAt.IsZero() && prev.BatteryMV > 0 {
		elapsed := cur.ReportedAt.Sub(prev.ReportedAt).Hours() / 24
		if elapsed > 0 {
			f.BatteryDropMVPerDay = float64(prev.BatteryMV-cur.BatteryMV) / elapsed
			f.HasHistory = true
		}
	}
	return f
}
