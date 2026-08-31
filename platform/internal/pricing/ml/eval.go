package ml

import (
	"fmt"
	"math"
	"sort"
)

// Metrics is the evaluation harness's verdict on one model against one holdout.
type Metrics struct {
	// MAE is the mean absolute error, in the target's units. It is the headline
	// number because a retail planner can read it directly: "the model is out
	// by 1.4 units a day on average".
	MAE float64 `json:"mae"`
	// RMSE punishes large errors quadratically. Reported alongside MAE because
	// the gap between them is itself informative: RMSE much larger than MAE
	// means the errors are concentrated in a few bad days, which is a different
	// problem from being uniformly slightly wrong.
	RMSE float64 `json:"rmse"`
	// MAPE is the mean absolute percentage error over rows with a non-zero
	// target. Zero-target rows are excluded and counted, because MAPE is
	// undefined there and quietly substituting a small denominator is how a
	// model with a 4% MAPE gets reported as having a 40,000% one.
	MAPE float64 `json:"mape"`
	// MAPEExcluded is how many rows were left out of the MAPE.
	MAPEExcluded int `json:"mape_excluded"`
	// Bias is the mean signed error. A model with a good MAE and a large bias
	// is systematically over- or under-forecasting, which in pricing shows up
	// as a consistent margin error rather than as noise.
	Bias float64 `json:"bias"`
	// R2 is the fraction of target variance explained, against the holdout's
	// own mean. A negative value means the model is worse than predicting the
	// holdout mean, which happens and should be visible.
	R2 float64 `json:"r_squared"`
	// Rows is the holdout size.
	Rows int `json:"rows"`
}

// Predictor is anything that scores a feature row. Both the float and the
// quantised ensembles satisfy it, which is what lets the harness evaluate them
// through exactly the same code path — the alternative is two evaluation
// implementations that eventually disagree.
type Predictor interface {
	Predict(x []float64) float64
}

// Evaluate scores a predictor against a holdout.
func Evaluate(p Predictor, X [][]float64, y []float64) (Metrics, error) {
	if len(X) == 0 || len(X) != len(y) {
		return Metrics{}, fmt.Errorf("%w: %d rows against %d targets", ErrTraining, len(X), len(y))
	}
	var absSum, sqSum, signedSum, pctSum float64
	excluded := 0
	for i, x := range X {
		e := p.Predict(x) - y[i]
		absSum += math.Abs(e)
		sqSum += e * e
		signedSum += e
		if y[i] != 0 {
			pctSum += math.Abs(e / y[i])
		} else {
			excluded++
		}
	}
	n := float64(len(X))
	m := Metrics{
		MAE: absSum / n, RMSE: math.Sqrt(sqSum / n),
		Bias: signedSum / n, Rows: len(X), MAPEExcluded: excluded,
	}
	if scored := len(X) - excluded; scored > 0 {
		m.MAPE = 100 * pctSum / float64(scored)
	}
	ybar := mean(y)
	var tss float64
	for _, v := range y {
		d := v - ybar
		tss += d * d
	}
	if tss > 0 {
		m.R2 = 1 - sqSum/tss
	}
	return m, nil
}

// Split partitions rows into a training and a holdout set.
//
// # Why the split is by position, not at random
//
// Demand data is a time series. A random split puts Tuesday in the training set
// and Wednesday in the holdout, and a model that has seen the surrounding days
// can interpolate the gap; the resulting holdout metric flatters the model by a
// factor that is invisible until it is deployed. The caller passes rows in time
// order and this function takes the tail as the holdout, which is the only
// split that measures what the model will actually be asked to do.
func Split(X [][]float64, y []float64, holdoutFraction float64) (trainX [][]float64, trainY []float64, testX [][]float64, testY []float64, err error) {
	if len(X) != len(y) {
		return nil, nil, nil, nil, fmt.Errorf("%w: %d rows against %d targets", ErrTraining, len(X), len(y))
	}
	if holdoutFraction <= 0 || holdoutFraction >= 1 {
		holdoutFraction = 0.2
	}
	cut := int(float64(len(X)) * (1 - holdoutFraction))
	if cut < 1 || cut >= len(X) {
		return nil, nil, nil, nil, fmt.Errorf("%w: %d rows cannot be split at %.0f%%", ErrTraining, len(X), holdoutFraction*100)
	}
	return X[:cut], y[:cut], X[cut:], y[cut:], nil
}

// ChampionChallenger is the outcome of comparing a candidate model against the
// model currently serving.
type ChampionChallenger struct {
	Champion   Metrics `json:"champion"`
	Challenger Metrics `json:"challenger"`
	// MAEImprovementPct is how much better the challenger's MAE is, as a
	// percentage of the champion's. Negative means worse.
	MAEImprovementPct float64 `json:"mae_improvement_pct"`
	// Promote is the recommendation.
	Promote bool `json:"promote"`
	// Rationale explains the recommendation in the terms an operator approving
	// the promotion needs.
	Rationale string `json:"rationale"`
}

// MinPromotionImprovementPct is the margin a challenger must beat the champion
// by before promotion is recommended.
//
// # Why a margin rather than "better is better"
//
// A holdout of a few hundred rows has a sampling error on MAE of several
// percent. Promoting on any improvement means promoting noise roughly half the
// time, and each promotion re-prices a store. Three percent is roughly twice the
// standard error of the MAE on the holdout sizes this platform trains on, and it
// is a knob rather than a constant of nature — it lives here so that changing
// it is a visible decision.
const MinPromotionImprovementPct = 3.0

// Compare evaluates a challenger against a champion on the same holdout.
//
// Both models are scored on identical rows. That is the whole point: two
// metrics computed on different holdouts are not comparable, and the most
// common way a worse model gets promoted is that it was evaluated on an easier
// week.
func Compare(champion, challenger Predictor, X [][]float64, y []float64) (ChampionChallenger, error) {
	cm, err := Evaluate(champion, X, y)
	if err != nil {
		return ChampionChallenger{}, err
	}
	hm, err := Evaluate(challenger, X, y)
	if err != nil {
		return ChampionChallenger{}, err
	}
	cc := ChampionChallenger{Champion: cm, Challenger: hm}
	if cm.MAE > 0 {
		cc.MAEImprovementPct = 100 * (cm.MAE - hm.MAE) / cm.MAE
	}
	switch {
	case hm.Rows < 30:
		cc.Rationale = fmt.Sprintf("holdout of %d rows is too small to distinguish the models", hm.Rows)
	case cc.MAEImprovementPct >= MinPromotionImprovementPct:
		cc.Promote = true
		cc.Rationale = fmt.Sprintf("challenger MAE %.4f beats champion %.4f by %.1f%%, above the %.1f%% bar",
			hm.MAE, cm.MAE, cc.MAEImprovementPct, MinPromotionImprovementPct)
	case cc.MAEImprovementPct > 0:
		cc.Rationale = fmt.Sprintf("challenger is only %.1f%% better, inside the noise of a %d-row holdout",
			cc.MAEImprovementPct, hm.Rows)
	default:
		cc.Rationale = fmt.Sprintf("challenger MAE %.4f is worse than champion %.4f", hm.MAE, cm.MAE)
	}
	return cc, nil
}

// EnsembleWeights blends two predictors' outputs.
//
// Tier 3 ensembles the LSTM with the GBT. The weight is chosen by a grid search
// on the holdout rather than fixed, because which model dominates depends on the
// SKU: a staple with strong weekly seasonality favours the recurrence, and a
// promotion-driven line favours the trees.
type EnsembleWeights struct {
	// WeightA is the weight on the first predictor; the second gets 1-WeightA.
	WeightA float64 `json:"weight_a"`
	// Metrics are the blend's holdout metrics at that weight.
	Metrics Metrics `json:"metrics"`
}

// FitEnsembleWeight grid-searches the blend weight that minimises holdout MAE.
func FitEnsembleWeight(a, b []float64, y []float64) (EnsembleWeights, error) {
	if len(a) != len(y) || len(b) != len(y) || len(y) == 0 {
		return EnsembleWeights{}, fmt.Errorf("%w: prediction lengths %d and %d against %d targets",
			ErrTraining, len(a), len(b), len(y))
	}
	best := EnsembleWeights{WeightA: 1, Metrics: Metrics{MAE: math.Inf(1)}}
	// 21 points at 0.05 spacing. A finer grid over a few hundred holdout rows
	// is fitting the holdout, not the blend.
	for i := 0; i <= 20; i++ {
		w := float64(i) / 20
		var absSum, sqSum, signed float64
		for j := range y {
			e := w*a[j] + (1-w)*b[j] - y[j]
			absSum += math.Abs(e)
			sqSum += e * e
			signed += e
		}
		n := float64(len(y))
		if mae := absSum / n; mae < best.Metrics.MAE {
			best = EnsembleWeights{
				WeightA: w,
				Metrics: Metrics{MAE: mae, RMSE: math.Sqrt(sqSum / n), Bias: signed / n, Rows: len(y)},
			}
		}
	}
	return best, nil
}

// Quantile returns the q-th quantile of v by linear interpolation. It is used
// by the anomaly service to pick a score threshold from an observed score
// distribution rather than from a guess.
func Quantile(v []float64, q float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := make([]float64, len(v))
	copy(c, v)
	sort.Float64s(c)
	if q <= 0 {
		return c[0]
	}
	if q >= 1 {
		return c[len(c)-1]
	}
	pos := q * float64(len(c)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return c[lo]
	}
	frac := pos - float64(lo)
	return c[lo]*(1-frac) + c[hi]*frac
}
