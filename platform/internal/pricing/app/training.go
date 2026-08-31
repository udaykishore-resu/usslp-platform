package app

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/usslp/usslp/platform/internal/pricing/domain"
	"github.com/usslp/usslp/platform/internal/pricing/features"
	"github.com/usslp/usslp/platform/internal/pricing/ml"
	"github.com/usslp/usslp/platform/internal/pricing/registry"
	"github.com/usslp/usslp/platform/pkg/canon"
)

// TrainingExample is one supervised row: the state of the world as it was known
// at DecisionAt, and the demand that followed.
type TrainingExample struct {
	// SKU and Store identify the row's subject.
	SKU   canon.SKU
	Store canon.StoreID
	// DecisionAt is the instant the row's features are assembled as of. It is
	// the moment a pricing decision would have been made.
	DecisionAt time.Time
	// Target is the demand observed in the period following DecisionAt.
	Target float64
}

// TrainingSet is the assembled matrix.
type TrainingSet struct {
	X [][]float64
	Y []float64
	// Rows describes each retained row, in the same order, for lineage.
	Rows []TrainingExample
	// Dropped counts rows discarded for missing features, with the reason.
	Dropped map[string]int
	// WindowStart and WindowEnd bracket the retained rows.
	WindowStart, WindowEnd time.Time
}

// ErrInsufficientData marks a training request that cannot produce a model.
var ErrInsufficientData = errors.New("pricing: insufficient training data")

// BuildTrainingSet assembles a point-in-time-correct training matrix.
//
// # The one rule that matters
//
// Every feature for a row is read with AsOf(row.DecisionAt). Nothing else in
// this function is subtle, and nothing else in it is as important. A row's
// features are the values the platform *knew* at the moment the decision would
// have been made — not the values that were true then and learned later, and
// certainly not the values known now. Rows are returned in DecisionAt order so
// that the holdout split is a forward split in time.
//
// Rows with missing features are dropped and counted rather than imputed. An
// imputed feature is a value the model treats as observed, and the imputation
// rule is itself a leak whenever it is computed from data the decision moment
// did not have.
func BuildTrainingSet(fs *features.Store, tenant canon.TenantID, examples []TrainingExample, featureNames []string) (TrainingSet, error) {
	if fs == nil {
		return TrainingSet{}, fmt.Errorf("%w: nil feature store", ErrInsufficientData)
	}
	if len(examples) == 0 {
		return TrainingSet{}, fmt.Errorf("%w: no examples", ErrInsufficientData)
	}
	ordered := make([]TrainingExample, len(examples))
	copy(ordered, examples)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].DecisionAt.Before(ordered[j].DecisionAt) })

	ts := TrainingSet{Dropped: map[string]int{}}
	for _, ex := range ordered {
		vec, missing, err := fs.Vector(tenant, ex.Store, ex.SKU, featureNames, ex.DecisionAt)
		if err != nil {
			return TrainingSet{}, err
		}
		if len(missing) > 0 {
			for _, m := range missing {
				ts.Dropped[m]++
			}
			continue
		}
		ts.X = append(ts.X, vec)
		ts.Y = append(ts.Y, ex.Target)
		ts.Rows = append(ts.Rows, ex)
	}
	if len(ts.Rows) == 0 {
		return ts, fmt.Errorf("%w: every row was dropped for missing features (%v)", ErrInsufficientData, ts.Dropped)
	}
	ts.WindowStart = ts.Rows[0].DecisionAt
	ts.WindowEnd = ts.Rows[len(ts.Rows)-1].DecisionAt
	return ts, nil
}

// TrainDemandModelInput configures a Tier-2 training run.
type TrainDemandModelInput struct {
	Slot registry.Slot
	Set  TrainingSet
	// Params configures the boosting. The zero value uses the platform
	// defaults.
	Params ml.GBTParams
	// HoldoutFraction is the tail of the data reserved for evaluation.
	HoldoutFraction float64
	// Notes is operator context recorded on the model.
	Notes string
}

// TrainDemandModelResult is what a training run produced.
type TrainDemandModelResult struct {
	Model *ml.GBT
	// Quantised is the int8 artefact for the edge.
	Quantised *ml.QuantisedGBT
	// Metrics are the float model's holdout metrics.
	Metrics ml.Metrics
	// Quantisation is the measured cost of the int8 conversion.
	Quantisation ml.QuantisationReport
	// Metadata is the registry entry, when a registry was supplied.
	Metadata registry.Metadata
	// Comparison is the verdict against the incumbent champion, when one
	// exists.
	Comparison *ml.ChampionChallenger
}

// MinTrainingRows is the smallest training set the service will fit a model to.
//
// Below this, a boosted ensemble memorises the data and its holdout metric is a
// coin flip. The number is a policy statement, not a mathematical bound: a store
// with less than this much history is served by the tenant-wide model and by
// Tier 1, which is a worse model and an honest one.
const MinTrainingRows = 120

// TrainDemandModel fits, evaluates, quantises and (when a registry is supplied)
// registers a Tier-2 demand model.
//
// It never promotes. Promotion is a separate, audited operator action, because
// the model that prices a store should change when a human decides it should.
func TrainDemandModel(reg *registry.Registry, in TrainDemandModelInput) (TrainDemandModelResult, error) {
	if len(in.Set.X) < MinTrainingRows {
		return TrainDemandModelResult{}, fmt.Errorf("%w: %d rows is below the %d-row minimum",
			ErrInsufficientData, len(in.Set.X), MinTrainingRows)
	}
	if in.HoldoutFraction <= 0 {
		in.HoldoutFraction = 0.2
	}
	trainX, trainY, testX, testY, err := ml.Split(in.Set.X, in.Set.Y, in.HoldoutFraction)
	if err != nil {
		return TrainDemandModelResult{}, err
	}
	model, err := ml.TrainGBT(trainX, trainY, in.Params)
	if err != nil {
		return TrainDemandModelResult{}, err
	}
	metrics, err := ml.Evaluate(model, testX, testY)
	if err != nil {
		return TrainDemandModelResult{}, err
	}
	quantised, err := ml.QuantiseGBT(model)
	if err != nil {
		return TrainDemandModelResult{}, err
	}
	qReport, err := ml.QuantisationDelta(model, quantised, testX, testY)
	if err != nil {
		return TrainDemandModelResult{}, err
	}

	res := TrainDemandModelResult{
		Model: model, Quantised: quantised,
		Metrics: metrics, Quantisation: qReport,
	}
	if reg == nil {
		return res, nil
	}

	body, err := model.MarshalBinary()
	if err != nil {
		return res, err
	}
	edge, err := quantised.MarshalBinary()
	if err != nil {
		return res, err
	}
	names := make([]string, domain.NumFeatures)
	copy(names, domain.FeatureNames[:])
	md, err := reg.Register(registry.Registration{
		Slot: in.Slot, Kind: ml.KindGBT, Body: body, EdgeBody: edge,
		Metrics: metrics, Quantisation: &qReport,
		TrainingRows: len(trainX), HoldoutRows: len(testX),
		TrainingWindowStart: in.Set.WindowStart, TrainingWindowEnd: in.Set.WindowEnd,
		FeatureNames: names,
		Hyperparameters: map[string]string{
			"rounds":           fmt.Sprint(in.Params.Rounds),
			"max_depth":        fmt.Sprint(in.Params.MaxDepth),
			"learning_rate":    fmt.Sprint(in.Params.LearningRate),
			"min_samples_leaf": fmt.Sprint(in.Params.MinSamplesLeaf),
			"l2":               fmt.Sprint(in.Params.L2),
			"bins":             fmt.Sprint(in.Params.Bins),
			"subsample":        fmt.Sprint(in.Params.Subsample),
			"seed":             fmt.Sprint(in.Params.Seed),
		},
		Notes: in.Notes,
	})
	if err != nil {
		return res, err
	}
	res.Metadata = md

	// Compare against the incumbent on the same holdout, so the operator sees
	// the verdict before deciding to promote.
	champion, _, err := reg.LoadChampionGBT(in.Slot, nil)
	if err == nil {
		cc, err := ml.Compare(champion, model, testX, testY)
		if err != nil {
			return res, err
		}
		res.Comparison = &cc
	} else if !errors.Is(err, registry.ErrNotFound) {
		return res, err
	}
	return res, nil
}

// EstimateElasticityFor reads a SKU's price/quantity history from the feature
// store, as of an instant, and fits the log-log model.
//
// The history is read point-in-time-correct for the same reason the training
// set is: a backfilled sales correction that arrived last week must not enter
// an elasticity estimate that is meant to reproduce what the platform believed
// at the time.
func EstimateElasticityFor(fs *features.Store, tenant canon.TenantID, store canon.StoreID, sku canon.SKU,
	asOf time.Time, limit int, policy ml.ElasticityPolicy) (ml.Elasticity, error) {

	priceHist, err := fs.History(features.Key{
		Tenant: tenant, Store: store, SKU: sku, Name: domain.FeatureNames[domain.FeatPrice],
	}, asOf, limit)
	if err != nil {
		return ml.Elasticity{}, err
	}
	unitsHist, err := fs.History(features.Key{
		Tenant: tenant, Store: store, SKU: sku, Name: FeatureUnitsSold,
	}, asOf, limit)
	if err != nil {
		return ml.Elasticity{}, err
	}

	// Join on validity time: the price in force during a period and the units
	// sold in that period. Joining on knowledge time instead would pair a price
	// with whichever sales figure happened to arrive alongside it, which for a
	// nightly sales feed and a real-time price feed is the wrong day's sales.
	units := make(map[int64]float64, len(unitsHist))
	for _, u := range unitsHist {
		key := u.ValidFrom.UTC().Truncate(24 * time.Hour).UnixNano()
		// Keep the most recently known value for a period; History returns
		// newest knowledge first, so the first write wins.
		if _, seen := units[key]; !seen {
			units[key] = u.Number
		}
	}
	obs := make([]ml.Observation, 0, len(priceHist))
	seen := make(map[int64]bool, len(priceHist))
	for _, p := range priceHist {
		key := p.ValidFrom.UTC().Truncate(24 * time.Hour).UnixNano()
		if seen[key] {
			continue
		}
		q, ok := units[key]
		if !ok {
			continue
		}
		seen[key] = true
		obs = append(obs, ml.Observation{PriceMinor: p.Number, Quantity: q})
	}
	return ml.EstimateElasticity(obs, policy)
}

// FeatureUnitsSold is the feature-store series name for realised demand. It is
// not part of the model input vector — it is the *target* — but it lives in the
// same store so that the elasticity estimator and the training-set builder read
// history through one code path.
const FeatureUnitsSold = "units_sold"
