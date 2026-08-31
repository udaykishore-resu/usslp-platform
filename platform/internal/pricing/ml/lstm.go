package ml

import (
	"fmt"
	"math"
)

// LSTM is a single-layer long short-term memory network with a linear read-out,
// used for Tier-3 demand time-series forecasting.
//
// # What is implemented
//
// Both inference and training. The forward pass is the standard four-gate LSTM
// cell; the backward pass is truncated backpropagation through time over the
// whole supplied sequence, with Adam and gradient clipping. The gradients are
// verified against central finite differences in the package's tests, which is
// the only honest way to claim a hand-written backward pass is correct.
//
// # Why an LSTM at Tier 3 and trees at Tier 2
//
// The two tiers answer different questions. Tier 2 asks "given today's state,
// what happens if I change this price?" — a tabular, low-order, causal-in-price
// question that gradient-boosted trees answer well in a millisecond. Tier 3 asks
// "what does the next fortnight of demand look like across every store?" — a
// sequence question with weekly and seasonal structure that a tabular model can
// only see through hand-built lag features. The recurrence carries that
// structure without the feature engineering, at a cost (tens of milliseconds
// per store, and a training run measured in minutes) that is fine for a job that
// runs every fifteen minutes in the cloud and unacceptable at the edge.
//
// # Layout
//
// Weights are stored as flat slices in gate-major order (input, forget, cell,
// output), which keeps one gate's row contiguous for the matrix-vector product
// and makes the serialised form a plain sequence of float32s.
type LSTM struct {
	// InputSize is the per-step feature width.
	InputSize int `json:"input_size"`
	// Hidden is the cell width.
	Hidden int `json:"hidden"`

	// Wx is Hidden*4 x InputSize, Wh is Hidden*4 x Hidden, B is Hidden*4.
	// Gate order is input, forget, cell candidate, output.
	Wx []float64 `json:"wx"`
	Wh []float64 `json:"wh"`
	B  []float64 `json:"b"`
	// Wy is 1 x Hidden and By is the scalar read-out bias. The head is a single
	// output because the platform forecasts one series — units per period for
	// one (store, SKU) — and a multi-output head would invite sharing a model
	// across SKUs whose demand has nothing in common.
	Wy []float64 `json:"wy"`
	By float64   `json:"by"`

	// InputMean and InputScale standardise the inputs. They are part of the
	// model rather than a preprocessing step the caller must remember, because
	// a forecast served with the wrong normalisation is silently wrong.
	InputMean  []float64 `json:"input_mean"`
	InputScale []float64 `json:"input_scale"`
	// TargetMean and TargetScale standardise the target the same way.
	TargetMean  float64 `json:"target_mean"`
	TargetScale float64 `json:"target_scale"`
}

// Gate offsets within the 4*Hidden blocks.
const (
	gateI = 0 // input gate
	gateF = 1 // forget gate
	gateG = 2 // cell candidate
	gateO = 3 // output gate
)

// NewLSTM allocates a network with deterministic small random weights.
//
// The forget-gate bias starts at 1 rather than 0. That is a well-established
// initialisation choice and it matters here specifically: with a zero bias the
// forget gate starts at 0.5 and the cell state decays by half per step, so a
// weekly seasonality in daily demand is attenuated by a factor of 128 before
// the gradient ever sees it.
func NewLSTM(inputSize, hidden int, seed uint64) (*LSTM, error) {
	if inputSize <= 0 || hidden <= 0 {
		return nil, fmt.Errorf("%w: LSTM needs a positive input size and hidden width", ErrTraining)
	}
	n := &LSTM{
		InputSize: inputSize, Hidden: hidden,
		Wx: make([]float64, 4*hidden*inputSize),
		Wh: make([]float64, 4*hidden*hidden),
		B:  make([]float64, 4*hidden),
		Wy: make([]float64, hidden),
		// Identity normalisation until Fit sets it.
		InputMean: make([]float64, inputSize), InputScale: ones(inputSize),
		TargetMean: 0, TargetScale: 1,
	}
	rng := newSplitMix(seed)
	// Xavier-style scaling keeps the pre-activations in the range where the
	// sigmoid and tanh have usable gradient.
	sx := math.Sqrt(1.0 / float64(inputSize))
	sh := math.Sqrt(1.0 / float64(hidden))
	for i := range n.Wx {
		n.Wx[i] = (rng.float64()*2 - 1) * sx
	}
	for i := range n.Wh {
		n.Wh[i] = (rng.float64()*2 - 1) * sh
	}
	for i := range n.Wy {
		n.Wy[i] = (rng.float64()*2 - 1) * sh
	}
	for h := 0; h < hidden; h++ {
		n.B[gateF*hidden+h] = 1.0
	}
	return n, nil
}

func ones(n int) []float64 {
	v := make([]float64, n)
	for i := range v {
		v[i] = 1
	}
	return v
}

// state holds the activations of one forward pass, retained for the backward
// pass. It is a value the caller owns so that inference — which never needs it
// — can run without allocating it.
type lstmState struct {
	// Per step: gates (4*Hidden), cell, hidden, tanh(cell).
	gates    [][]float64
	cell     [][]float64
	hidden   [][]float64
	tanhCell [][]float64
	inputs   [][]float64
}

// Forward runs the sequence and returns the read-out at every step.
//
// Returning every step rather than only the last is what lets the training loop
// use a sequence of length T as T-1 supervised examples instead of one, which
// matters a great deal when a store-SKU series is 400 daily observations long.
func (n *LSTM) Forward(seq [][]float64) ([]float64, error) {
	out, _, err := n.forward(seq, false)
	return out, err
}

// PredictNext runs the sequence and returns only the final read-out, in the
// target's original units. This is the Tier-3 inference entry point.
func (n *LSTM) PredictNext(seq [][]float64) (float64, error) {
	out, err := n.Forward(seq)
	if err != nil {
		return 0, err
	}
	if len(out) == 0 {
		return 0, fmt.Errorf("%w: empty sequence", ErrTraining)
	}
	return out[len(out)-1], nil
}

func (n *LSTM) forward(seq [][]float64, keep bool) ([]float64, *lstmState, error) {
	if len(seq) == 0 {
		return nil, nil, fmt.Errorf("%w: empty sequence", ErrTraining)
	}
	H := n.Hidden
	out := make([]float64, len(seq))
	var st *lstmState
	if keep {
		st = &lstmState{
			gates: make([][]float64, len(seq)), cell: make([][]float64, len(seq)),
			hidden: make([][]float64, len(seq)), tanhCell: make([][]float64, len(seq)),
			inputs: make([][]float64, len(seq)),
		}
	}
	h := make([]float64, H)
	c := make([]float64, H)
	g := make([]float64, 4*H)
	x := make([]float64, n.InputSize)

	for t, raw := range seq {
		if len(raw) != n.InputSize {
			return nil, nil, fmt.Errorf("%w: step %d has width %d, expected %d", ErrTraining, t, len(raw), n.InputSize)
		}
		for i := range x {
			x[i] = (raw[i] - n.InputMean[i]) / n.InputScale[i]
		}
		copy(g, n.B)
		for k := 0; k < 4*H; k++ {
			row := n.Wx[k*n.InputSize : (k+1)*n.InputSize]
			s := 0.0
			for i, v := range x {
				s += row[i] * v
			}
			rowH := n.Wh[k*H : (k+1)*H]
			for i, v := range h {
				s += rowH[i] * v
			}
			g[k] += s
		}
		// Apply the non-linearities in place: sigmoid on i, f, o; tanh on g.
		for hh := 0; hh < H; hh++ {
			g[gateI*H+hh] = sigmoid(g[gateI*H+hh])
			g[gateF*H+hh] = sigmoid(g[gateF*H+hh])
			g[gateG*H+hh] = math.Tanh(g[gateG*H+hh])
			g[gateO*H+hh] = sigmoid(g[gateO*H+hh])
		}
		newC := make([]float64, H)
		newH := make([]float64, H)
		tc := make([]float64, H)
		for hh := 0; hh < H; hh++ {
			newC[hh] = g[gateF*H+hh]*c[hh] + g[gateI*H+hh]*g[gateG*H+hh]
			tc[hh] = math.Tanh(newC[hh])
			newH[hh] = g[gateO*H+hh] * tc[hh]
		}
		y := n.By
		for hh := 0; hh < H; hh++ {
			y += n.Wy[hh] * newH[hh]
		}
		out[t] = y*n.TargetScale + n.TargetMean

		if keep {
			gc := make([]float64, 4*H)
			copy(gc, g)
			xc := make([]float64, n.InputSize)
			copy(xc, x)
			st.gates[t], st.cell[t], st.hidden[t], st.tanhCell[t], st.inputs[t] = gc, newC, newH, tc, xc
		}
		c, h = newC, newH
	}
	return out, st, nil
}

// LSTMTrainParams configures training.
type LSTMTrainParams struct {
	// Epochs is the number of passes over the training sequences.
	Epochs int
	// LearningRate is Adam's step size.
	LearningRate float64
	// ClipNorm bounds the global gradient norm. Recurrent networks on demand
	// series produce occasional very large gradients at promotion spikes, and
	// one unclipped step destroys a trained model.
	ClipNorm float64
	// Beta1, Beta2, Eps are Adam's moments and epsilon.
	Beta1, Beta2, Eps float64
	// WarmupSteps are ignored at the start of each sequence when computing the
	// loss, so the model is not penalised for predictions made before the
	// recurrence has any history to work with.
	WarmupSteps int
}

// DefaultLSTMTrainParams are sensible values for a demand series measured in
// hundreds of daily observations.
func DefaultLSTMTrainParams() LSTMTrainParams {
	return LSTMTrainParams{
		Epochs: 200, LearningRate: 0.02, ClipNorm: 5,
		Beta1: 0.9, Beta2: 0.999, Eps: 1e-8, WarmupSteps: 3,
	}
}

func (p *LSTMTrainParams) applyDefaults() {
	d := DefaultLSTMTrainParams()
	if p.Epochs <= 0 {
		p.Epochs = d.Epochs
	}
	if p.LearningRate <= 0 {
		p.LearningRate = d.LearningRate
	}
	if p.ClipNorm <= 0 {
		p.ClipNorm = d.ClipNorm
	}
	if p.Beta1 <= 0 {
		p.Beta1 = d.Beta1
	}
	if p.Beta2 <= 0 {
		p.Beta2 = d.Beta2
	}
	if p.Eps <= 0 {
		p.Eps = d.Eps
	}
	if p.WarmupSteps < 0 {
		p.WarmupSteps = 0
	}
}

// Sequence is one training example: a series of per-step inputs and the target
// at each step.
type Sequence struct {
	// Inputs[t] is the feature vector observed at step t.
	Inputs [][]float64
	// Targets[t] is the value to predict at step t, normally the next period's
	// demand.
	Targets []float64
}

// Fit trains the network by truncated backpropagation through time.
//
// Normalisation statistics are computed from the training set and stored on the
// model, so a caller cannot forget to apply them at inference.
func (n *LSTM) Fit(seqs []Sequence, p LSTMTrainParams) ([]float64, error) {
	p.applyDefaults()
	if len(seqs) == 0 {
		return nil, fmt.Errorf("%w: no training sequences", ErrTraining)
	}
	for i, s := range seqs {
		if len(s.Inputs) != len(s.Targets) {
			return nil, fmt.Errorf("%w: sequence %d has %d inputs and %d targets",
				ErrTraining, i, len(s.Inputs), len(s.Targets))
		}
		if len(s.Inputs) <= p.WarmupSteps {
			return nil, fmt.Errorf("%w: sequence %d is shorter than the %d warm-up steps",
				ErrTraining, i, p.WarmupSteps)
		}
	}
	n.fitNormalisation(seqs)

	adam := newAdam(n, p)
	losses := make([]float64, 0, p.Epochs)
	for epoch := 0; epoch < p.Epochs; epoch++ {
		g := newLSTMGrads(n)
		total, count := 0.0, 0
		for _, s := range seqs {
			loss, k, err := n.accumulate(s, p.WarmupSteps, g)
			if err != nil {
				return nil, err
			}
			total += loss
			count += k
		}
		if count == 0 {
			return nil, fmt.Errorf("%w: every step fell inside the warm-up window", ErrTraining)
		}
		g.scale(1 / float64(count))
		g.clip(p.ClipNorm)
		adam.step(n, g)
		losses = append(losses, total/float64(count))
	}
	return losses, nil
}

// fitNormalisation computes per-feature standardisation from the training data.
func (n *LSTM) fitNormalisation(seqs []Sequence) {
	mean := make([]float64, n.InputSize)
	m2 := make([]float64, n.InputSize)
	count := 0
	var tMean, tM2 float64
	for _, s := range seqs {
		for t, x := range s.Inputs {
			count++
			for i, v := range x {
				d := v - mean[i]
				mean[i] += d / float64(count)
				m2[i] += d * (v - mean[i])
			}
			dt := s.Targets[t] - tMean
			tMean += dt / float64(count)
			tM2 += dt * (s.Targets[t] - tMean)
		}
	}
	if count < 2 {
		return
	}
	for i := range mean {
		sd := math.Sqrt(m2[i] / float64(count-1))
		if sd < 1e-9 {
			// A constant feature: leaving the scale at one keeps the centred
			// value at zero rather than dividing by a rounding error.
			sd = 1
		}
		n.InputMean[i], n.InputScale[i] = mean[i], sd
	}
	tsd := math.Sqrt(tM2 / float64(count-1))
	if tsd < 1e-9 {
		tsd = 1
	}
	n.TargetMean, n.TargetScale = tMean, tsd
}

// lstmGrads accumulates parameter gradients.
type lstmGrads struct {
	Wx, Wh, B, Wy []float64
	By            float64
}

func newLSTMGrads(n *LSTM) *lstmGrads {
	return &lstmGrads{
		Wx: make([]float64, len(n.Wx)), Wh: make([]float64, len(n.Wh)),
		B: make([]float64, len(n.B)), Wy: make([]float64, len(n.Wy)),
	}
}

func (g *lstmGrads) scale(f float64) {
	for i := range g.Wx {
		g.Wx[i] *= f
	}
	for i := range g.Wh {
		g.Wh[i] *= f
	}
	for i := range g.B {
		g.B[i] *= f
	}
	for i := range g.Wy {
		g.Wy[i] *= f
	}
	g.By *= f
}

func (g *lstmGrads) clip(maxNorm float64) {
	sum := 0.0
	for _, v := range g.Wx {
		sum += v * v
	}
	for _, v := range g.Wh {
		sum += v * v
	}
	for _, v := range g.B {
		sum += v * v
	}
	for _, v := range g.Wy {
		sum += v * v
	}
	sum += g.By * g.By
	norm := math.Sqrt(sum)
	if norm <= maxNorm || norm == 0 {
		return
	}
	g.scale(maxNorm / norm)
}

// accumulate runs one sequence forward and back, adding to g. It returns the
// summed squared loss in normalised target units and the number of supervised
// steps that contributed.
func (n *LSTM) accumulate(s Sequence, warmup int, g *lstmGrads) (float64, int, error) {
	_, st, err := n.forward(s.Inputs, true)
	if err != nil {
		return 0, 0, err
	}
	H, T := n.Hidden, len(s.Inputs)

	// dy[t] is the derivative of the loss with respect to the *normalised*
	// read-out at step t. Working in normalised units throughout keeps the
	// gradient scale independent of the units the retailer counts in.
	dy := make([]float64, T)
	loss, count := 0.0, 0
	for t := warmup; t < T; t++ {
		yhat := n.By
		for hh := 0; hh < H; hh++ {
			yhat += n.Wy[hh] * st.hidden[t][hh]
		}
		target := (s.Targets[t] - n.TargetMean) / n.TargetScale
		diff := yhat - target
		loss += 0.5 * diff * diff
		dy[t] = diff
		count++
	}

	dhNext := make([]float64, H)
	dcNext := make([]float64, H)
	dgate := make([]float64, 4*H)
	dh := make([]float64, H)

	for t := T - 1; t >= 0; t-- {
		// Read-out contribution.
		copy(dh, dhNext)
		if dy[t] != 0 {
			for hh := 0; hh < H; hh++ {
				g.Wy[hh] += dy[t] * st.hidden[t][hh]
				dh[hh] += dy[t] * n.Wy[hh]
			}
			g.By += dy[t]
		}

		gates := st.gates[t]
		tc := st.tanhCell[t]
		var prevC []float64
		if t > 0 {
			prevC = st.cell[t-1]
		} else {
			prevC = make([]float64, H)
		}

		for hh := 0; hh < H; hh++ {
			o := gates[gateO*H+hh]
			i := gates[gateI*H+hh]
			f := gates[gateF*H+hh]
			cg := gates[gateG*H+hh]

			do := dh[hh] * tc[hh]
			dc := dh[hh]*o*(1-tc[hh]*tc[hh]) + dcNext[hh]

			di := dc * cg
			dcg := dc * i
			df := dc * prevC[hh]

			// Chain through the activations: sigmoid' = s(1-s), tanh' = 1-t^2.
			dgate[gateI*H+hh] = di * i * (1 - i)
			dgate[gateF*H+hh] = df * f * (1 - f)
			dgate[gateG*H+hh] = dcg * (1 - cg*cg)
			dgate[gateO*H+hh] = do * o * (1 - o)

			dcNext[hh] = dc * f
		}

		x := st.inputs[t]
		var prevH []float64
		if t > 0 {
			prevH = st.hidden[t-1]
		} else {
			prevH = make([]float64, H)
		}
		for hh := range dhNext {
			dhNext[hh] = 0
		}
		for k := 0; k < 4*H; k++ {
			d := dgate[k]
			if d == 0 {
				continue
			}
			g.B[k] += d
			rowX := g.Wx[k*n.InputSize : (k+1)*n.InputSize]
			for i, v := range x {
				rowX[i] += d * v
			}
			rowH := g.Wh[k*H : (k+1)*H]
			wRowH := n.Wh[k*H : (k+1)*H]
			for i, v := range prevH {
				rowH[i] += d * v
				dhNext[i] += d * wRowH[i]
			}
		}
	}
	return loss, count, nil
}

// adam is the optimiser state.
type adam struct {
	p                      LSTMTrainParams
	mWx, vWx               []float64
	mWh, vWh               []float64
	mB, vB                 []float64
	mWy, vWy               []float64
	mBy, vBy               float64
	t                      int
	beta1Pow, beta2PowInit float64
}

func newAdam(n *LSTM, p LSTMTrainParams) *adam {
	return &adam{
		p:   p,
		mWx: make([]float64, len(n.Wx)), vWx: make([]float64, len(n.Wx)),
		mWh: make([]float64, len(n.Wh)), vWh: make([]float64, len(n.Wh)),
		mB: make([]float64, len(n.B)), vB: make([]float64, len(n.B)),
		mWy: make([]float64, len(n.Wy)), vWy: make([]float64, len(n.Wy)),
	}
}

func (a *adam) step(n *LSTM, g *lstmGrads) {
	a.t++
	b1t := 1 - math.Pow(a.p.Beta1, float64(a.t))
	b2t := 1 - math.Pow(a.p.Beta2, float64(a.t))
	upd := func(w, grad, m, v []float64) {
		for i := range w {
			m[i] = a.p.Beta1*m[i] + (1-a.p.Beta1)*grad[i]
			v[i] = a.p.Beta2*v[i] + (1-a.p.Beta2)*grad[i]*grad[i]
			mh := m[i] / b1t
			vh := v[i] / b2t
			w[i] -= a.p.LearningRate * mh / (math.Sqrt(vh) + a.p.Eps)
		}
	}
	upd(n.Wx, g.Wx, a.mWx, a.vWx)
	upd(n.Wh, g.Wh, a.mWh, a.vWh)
	upd(n.B, g.B, a.mB, a.vB)
	upd(n.Wy, g.Wy, a.mWy, a.vWy)

	a.mBy = a.p.Beta1*a.mBy + (1-a.p.Beta1)*g.By
	a.vBy = a.p.Beta2*a.vBy + (1-a.p.Beta2)*g.By*g.By
	n.By -= a.p.LearningRate * (a.mBy / b1t) / (math.Sqrt(a.vBy/b2t) + a.p.Eps)
}

// Loss returns the mean squared error of the model on a sequence, in the
// target's original units. It is what the evaluation harness calls.
func (n *LSTM) Loss(s Sequence, warmup int) (float64, error) {
	out, err := n.Forward(s.Inputs)
	if err != nil {
		return 0, err
	}
	sum, count := 0.0, 0
	for t := warmup; t < len(out); t++ {
		d := out[t] - s.Targets[t]
		sum += d * d
		count++
	}
	if count == 0 {
		return 0, fmt.Errorf("%w: no scored steps", ErrTraining)
	}
	return sum / float64(count), nil
}

// Parameters returns every weight as one flat slice, aliasing the model, so a
// gradient check can perturb parameters generically. It is exported for the
// tests that verify the backward pass, which is the only way to substantiate a
// claim that a hand-written BPTT is correct.
func (n *LSTM) Parameters() [][]float64 {
	return [][]float64{n.Wx, n.Wh, n.B, n.Wy}
}

func sigmoid(x float64) float64 {
	// Split on the sign to keep exp's argument non-positive; exp of a large
	// positive number overflows to +Inf and turns the whole forward pass into
	// NaN, which on a demand series with an outlier is not hypothetical.
	if x >= 0 {
		return 1 / (1 + math.Exp(-x))
	}
	e := math.Exp(x)
	return e / (1 + e)
}
