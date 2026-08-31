package app

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/usslp/usslp/platform/internal/pricing/domain"
	"github.com/usslp/usslp/platform/internal/pricing/features"
	"github.com/usslp/usslp/platform/internal/pricing/ml"
	"github.com/usslp/usslp/platform/internal/pricing/registry"
	"github.com/usslp/usslp/platform/pkg/canon"
	"github.com/usslp/usslp/platform/pkg/kvstore"
)

const (
	testTenant = canon.TenantID("acme")
	testStore  = canon.StoreID("store-001")
	testSKU    = canon.SKU("sku-42")
)

func newFeatureStore(t *testing.T) (*features.Store, *kvstore.Store) {
	t.Helper()
	kv, err := kvstore.OpenWith(kvstore.Options{Dir: t.TempDir(), Sync: kvstore.SyncNever})
	if err != nil {
		t.Fatalf("open kv: %v", err)
	}
	t.Cleanup(func() { _ = kv.Close() })
	fs, err := features.New(features.Config{KV: kv})
	if err != nil {
		t.Fatalf("new feature store: %v", err)
	}
	return fs, kv
}

func featureNames() []string {
	names := make([]string, domain.NumFeatures)
	copy(names, domain.FeatureNames[:])
	return names
}

// seedSyntheticHistory writes a year of daily observations for one SKU, with
// demand generated from a known elasticity, and returns the training examples
// that go with them.
//
// The knowledge lag is the point: every feature for day d is only *known* at
// 06:00 on day d, and each training row is assembled as of 05:00 on day d — so
// a leaking implementation sees a day it should not.
func seedSyntheticHistory(t *testing.T, fs *features.Store, days int, elasticity float64) []TrainingExample {
	t.Helper()
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	rng := uint64(20260830)
	next := func() float64 {
		rng += 0x9E3779B97F4A7C15
		z := rng
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		return float64(z>>11) / float64(1<<53)
	}
	prices := []float64{179, 189, 199, 209, 219, 229, 239}
	examples := make([]TrainingExample, 0, days)

	for d := 0; d < days; d++ {
		day := start.AddDate(0, 0, d)
		known := day.Add(6 * time.Hour)
		price := prices[d%len(prices)]
		units := 40 * math.Pow(price/209, elasticity) * (0.9 + 0.2*next())

		recs := []features.Record{
			{Key: fkey(domain.FeatureNames[domain.FeatPrice]),
				Value: features.Value{Number: price, ValidFrom: day, KnownAt: known}},
			{Key: fkey(FeatureUnitsSold),
				Value: features.Value{Number: units, ValidFrom: day, KnownAt: known}},
			{Key: fkey(domain.FeatureNames[domain.FeatHourOfDay]),
				Value: features.Value{Number: 9, ValidFrom: day, KnownAt: known}},
			{Key: fkey(domain.FeatureNames[domain.FeatDayOfWeek]),
				Value: features.Value{Number: float64(int(day.Weekday())), ValidFrom: day, KnownAt: known}},
			{Key: fkey(domain.FeatureNames[domain.FeatDaysToExpiry]),
				Value: features.Value{Number: 6, ValidFrom: day, KnownAt: known}},
			{Key: fkey(domain.FeatureNames[domain.FeatSeason]),
				Value: features.Value{Number: float64(domain.Season(day, false)), ValidFrom: day, KnownAt: known}},
			{Key: fkey(domain.FeatureNames[domain.FeatInventoryLevel]),
				Value: features.Value{Number: 120 + 40*next(), ValidFrom: day, KnownAt: known}},
			{Key: fkey(domain.FeatureNames[domain.FeatDaysOfSupply]),
				Value: features.Value{Number: 3 + next(), ValidFrom: day, KnownAt: known}},
			{Key: fkey(domain.FeatureNames[domain.FeatWasteRate]),
				Value: features.Value{Number: 0.02 * next(), ValidFrom: day, KnownAt: known}},
			{Key: fkey(domain.FeatureNames[domain.FeatCompetitorPrice]),
				Value: features.Value{Number: price * (0.95 + 0.1*next()), ValidFrom: day, KnownAt: known}},
			{Key: fkey(domain.FeatureNames[domain.FeatVelocity7]),
				Value: features.Value{Number: units, ValidFrom: day, KnownAt: known}},
			{Key: fkey(domain.FeatureNames[domain.FeatVelocity14]),
				Value: features.Value{Number: units, ValidFrom: day, KnownAt: known}},
			{Key: fkey(domain.FeatureNames[domain.FeatVelocity30]),
				Value: features.Value{Number: units, ValidFrom: day, KnownAt: known}},
			{Key: fkey(domain.FeatureNames[domain.FeatElasticity]),
				Value: features.Value{Number: elasticity, ValidFrom: day, KnownAt: known}},
			{Key: fkey(domain.FeatureNames[domain.FeatWeatherBucket]),
				Value: features.Value{Number: float64(d % 4), ValidFrom: day, KnownAt: known}},
			{Key: fkey(domain.FeatureNames[domain.FeatLocalEvent]),
				Value: features.Value{Number: 0, ValidFrom: day, KnownAt: known}},
		}
		if err := fs.PutBatch(recs); err != nil {
			t.Fatalf("seed day %d: %v", d, err)
		}
		examples = append(examples, TrainingExample{
			SKU: testSKU, Store: testStore,
			// 07:00 — an hour after the day's features became known.
			DecisionAt: day.Add(7 * time.Hour),
			Target:     units,
		})
	}
	return examples
}

func fkey(name string) features.Key {
	return features.Key{Tenant: testTenant, Store: testStore, SKU: testSKU, Name: name}
}

// TestBuildTrainingSetIsPointInTimeCorrect is the leakage test at the
// training-set level: rows assembled before their own features were known must
// be dropped, not silently filled with the values that arrived later.
func TestBuildTrainingSetIsPointInTimeCorrect(t *testing.T) {
	fs, _ := newFeatureStore(t)
	examples := seedSyntheticHistory(t, fs, 60, -1.8)

	t.Run("rows assembled after the features are known are kept", func(t *testing.T) {
		set, err := BuildTrainingSet(fs, testTenant, examples, featureNames())
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if len(set.X) != len(examples) {
			t.Fatalf("kept %d of %d rows, dropped %v", len(set.X), len(examples), set.Dropped)
		}
		if !set.WindowStart.Before(set.WindowEnd) {
			t.Errorf("training window is %s..%s", set.WindowStart, set.WindowEnd)
		}
	})

	t.Run("rows assembled an hour too early are dropped", func(t *testing.T) {
		early := make([]TrainingExample, len(examples))
		copy(early, examples)
		for i := range early {
			// 05:00, an hour before the day's features are learned.
			early[i].DecisionAt = early[i].DecisionAt.Add(-2 * time.Hour)
		}
		set, err := BuildTrainingSet(fs, testTenant, early, featureNames())
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		// Every row loses its own day's features. The first row has no earlier
		// day to fall back on and is dropped outright; the rest fall back to
		// the previous day, which is correct point-in-time behaviour and must
		// not silently become the current day's values.
		if len(set.X) >= len(early) {
			t.Errorf("kept %d of %d rows: nothing was dropped, so a row saw features it could not have had",
				len(set.X), len(early))
		}
		for i, row := range set.X {
			// The price the row carries must be the *previous* trading day's,
			// never the day the row is dated.
			ex := set.Rows[i]
			want, err := fs.AsOf(fkey(domain.FeatureNames[domain.FeatPrice]), ex.DecisionAt)
			if err != nil {
				t.Fatalf("as-of: %v", err)
			}
			if row[domain.FeatPrice] != want.Number {
				t.Errorf("row %d carries price %v, want the %v known at %s",
					i, row[domain.FeatPrice], want.Number, ex.DecisionAt)
			}
			if want.KnownAt.After(ex.DecisionAt) {
				t.Fatalf("row %d used a value known at %s, after its decision instant %s",
					i, want.KnownAt, ex.DecisionAt)
			}
		}
	})

	t.Run("rows before any data exists are all dropped", func(t *testing.T) {
		ancient := []TrainingExample{{
			SKU: testSKU, Store: testStore,
			DecisionAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), Target: 10,
		}}
		_, err := BuildTrainingSet(fs, testTenant, ancient, featureNames())
		if !errors.Is(err, ErrInsufficientData) {
			t.Errorf("err = %v, want ErrInsufficientData", err)
		}
	})
}

// TestTrainDemandModelEndToEnd trains, evaluates, quantises and registers a
// model from the synthetic history, and reports the measured numbers.
func TestTrainDemandModelEndToEnd(t *testing.T) {
	fs, kv := newFeatureStore(t)
	examples := seedSyntheticHistory(t, fs, 400, -1.8)
	set, err := BuildTrainingSet(fs, testTenant, examples, featureNames())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	reg, err := registry.New(kv)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	slot := registry.Slot{Tenant: testTenant, Store: testStore, Purpose: registry.PurposeDemand}

	res, err := TrainDemandModel(reg, TrainDemandModelInput{
		Slot: slot, Set: set,
		Params: ml.GBTParams{Rounds: 150, MaxDepth: 4, LearningRate: 0.08, Seed: 5},
		Notes:  "synthetic history",
	})
	if err != nil {
		t.Fatalf("train: %v", err)
	}
	t.Logf("synthetic store history (%d rows, %d holdout): MAE %.4f RMSE %.4f MAPE %.2f%% R2 %.4f; "+
		"int8 delta %+.4f (%+.2f%%), %d -> %d bytes (%.1fx)",
		res.Metadata.TrainingRows, res.Metrics.Rows, res.Metrics.MAE, res.Metrics.RMSE,
		res.Metrics.MAPE, res.Metrics.R2,
		res.Quantisation.MAEDelta, res.Quantisation.MAEDeltaPct,
		res.Quantisation.FloatBytes, res.Quantisation.Int8Bytes, res.Quantisation.CompressionRatio)

	if res.Metadata.Stage != registry.StageChallenger {
		t.Errorf("a freshly trained model is at stage %q, want challenger", res.Metadata.Stage)
	}
	if res.Comparison != nil {
		t.Errorf("there is no champion yet, so there should be no comparison: %+v", res.Comparison)
	}

	// Nothing serves until an operator promotes.
	if _, err := reg.Champion(slot); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("a model became champion without a promotion: %v", err)
	}
	if _, err := reg.Promote(res.Metadata.ID, registry.PromoteOptions{}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	champ, _, err := reg.LoadChampionGBT(slot, featureNames())
	if err != nil {
		t.Fatalf("load champion: %v", err)
	}
	// The reloaded model must predict identically to the one in memory.
	for _, x := range set.X[:20] {
		if a, b := res.Model.Predict(x), champ.Predict(x); a != b {
			t.Fatalf("the reloaded champion predicts %v where the trained model predicts %v", b, a)
		}
	}
}

func TestTrainDemandModelRefusesThinData(t *testing.T) {
	fs, kv := newFeatureStore(t)
	examples := seedSyntheticHistory(t, fs, 40, -1.8)
	set, err := BuildTrainingSet(fs, testTenant, examples, featureNames())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	reg, err := registry.New(kv)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	_, err = TrainDemandModel(reg, TrainDemandModelInput{
		Slot: registry.Slot{Tenant: testTenant, Store: testStore, Purpose: registry.PurposeDemand},
		Set:  set,
	})
	if !errors.Is(err, ErrInsufficientData) {
		t.Errorf("err = %v, want a refusal on %d rows", err, len(set.X))
	}
}

// TestElasticityForRecoversTheSeededValue closes the loop from the feature
// store: the estimator must recover the elasticity the synthetic history was
// generated from.
func TestElasticityForRecoversTheSeededValue(t *testing.T) {
	const truth = -1.8
	fs, _ := newFeatureStore(t)
	seedSyntheticHistory(t, fs, 200, truth)

	asOf := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e, err := EstimateElasticityFor(fs, testTenant, testStore, testSKU, asOf, 400, ml.DefaultElasticityPolicy())
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if !e.Usable {
		t.Fatalf("estimate unusable: %s (%+v)", e.Reason, e)
	}
	if math.Abs(e.Coefficient-truth) > 0.2 {
		t.Errorf("recovered %.4f from the feature store, want %.2f within 0.2", e.Coefficient, truth)
	}
	t.Logf("synthetic history: seeded elasticity %.2f, recovered %.4f, 95%% CI [%.3f, %.3f] from %d observations",
		truth, e.Coefficient, e.Low, e.High, e.Observations)
}

// TestElasticityForRespectsTheAsOfCut proves the estimator cannot see history it
// should not: an as-of instant early in the series must produce an estimate
// resting on fewer observations than one at the end.
func TestElasticityForRespectsTheAsOfCut(t *testing.T) {
	fs, _ := newFeatureStore(t)
	seedSyntheticHistory(t, fs, 200, -1.8)

	early, err := EstimateElasticityFor(fs, testTenant, testStore, testSKU,
		time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC), 400, ml.DefaultElasticityPolicy())
	if err != nil {
		t.Fatalf("early: %v", err)
	}
	late, err := EstimateElasticityFor(fs, testTenant, testStore, testSKU,
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 400, ml.DefaultElasticityPolicy())
	if err != nil {
		t.Fatalf("late: %v", err)
	}
	if early.Observations >= late.Observations {
		t.Errorf("an estimate as of February used %d observations, one as of the following January used %d",
			early.Observations, late.Observations)
	}
	if early.Observations > 32 {
		t.Errorf("as of 1 February the series is 31 days long, but the estimate used %d observations",
			early.Observations)
	}
}
