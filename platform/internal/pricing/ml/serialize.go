package ml

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
)

// The USSLP model container.
//
// # Why not JSON, gob or protobuf
//
// The Store Gateway Unit loads models from a flash partition on every boot and
// after every model push, with a hard requirement that a half-written file is
// detected rather than parsed into a plausible-looking model. JSON of a 120-tree
// float ensemble is about 2 MB and takes tens of milliseconds to parse; gob
// binds the wire format to Go's reflection and to the exact struct definitions,
// so a field rename becomes an un-loadable fleet; protobuf is a dependency this
// module does not have and cannot fetch.
//
// This format is a fixed header, a length-prefixed body, and a CRC-32 over
// everything before it. It decodes in one pass with a single allocation per
// slice, and a truncated or corrupted file fails at the checksum instead of at
// whichever field happened to be short.
const (
	// modelMagic identifies the container.
	modelMagic = "USML"
	// modelFormatVersion is the container layout version. A reader refuses a
	// version it does not recognise: guessing at field offsets across a fleet
	// of 50 million labels' worth of gateways is not a recoverable mistake.
	modelFormatVersion uint16 = 1
)

// ModelKind identifies the payload type inside the container.
type ModelKind uint16

// The serialisable model kinds.
const (
	KindGBT ModelKind = 1
	// KindQuantisedGBT is the int8 edge form.
	KindQuantisedGBT ModelKind = 2
	KindLSTM         ModelKind = 3
	KindIsoForest    ModelKind = 4
)

// String renders the kind for metadata and error messages.
func (k ModelKind) String() string {
	switch k {
	case KindGBT:
		return "gbt"
	case KindQuantisedGBT:
		return "gbt_int8"
	case KindLSTM:
		return "lstm"
	case KindIsoForest:
		return "isolation_forest"
	}
	return fmt.Sprintf("unknown(%d)", uint16(k))
}

// ErrModelFormat marks a container that cannot be trusted.
var ErrModelFormat = errors.New("ml: malformed model container")

// writer is an append-only encoder. Errors cannot occur while appending to a
// slice, which is why the encode path has no error returns until the very end.
type writer struct{ b []byte }

func (w *writer) u8(v uint8)   { w.b = append(w.b, v) }
func (w *writer) u16(v uint16) { w.b = binary.LittleEndian.AppendUint16(w.b, v) }
func (w *writer) u32(v uint32) { w.b = binary.LittleEndian.AppendUint32(w.b, v) }
func (w *writer) i32(v int32)  { w.u32(uint32(v)) }
func (w *writer) f64(v float64) {
	w.b = binary.LittleEndian.AppendUint64(w.b, math.Float64bits(v))
}

// f32 stores a float as 32 bits.
//
// LSTM weights and normalisation statistics are stored at single precision:
// they are the output of a stochastic optimiser whose run-to-run variation is
// far larger than 2^-24, so the second half of every mantissa is noise being
// paid for twice.
func (w *writer) f32(v float64) {
	w.u32(math.Float32bits(float32(v)))
}

func (w *writer) str(s string) {
	w.u16(uint16(len(s)))
	w.b = append(w.b, s...)
}

// mreader mirrors writer with bounds checking.
type mreader struct {
	b   []byte
	err error
}

func (r *mreader) fail(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf("%w: "+format, append([]any{ErrModelFormat}, args...)...)
	}
}

func (r *mreader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || len(r.b) < n {
		r.fail("truncated: wanted %d bytes, have %d", n, len(r.b))
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *mreader) u8() uint8 {
	b := r.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (r *mreader) u16() uint16 {
	b := r.take(2)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

func (r *mreader) u32() uint32 {
	b := r.take(4)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (r *mreader) i32() int32 { return int32(r.u32()) }

func (r *mreader) f64() float64 {
	b := r.take(8)
	if b == nil {
		return 0
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(b))
}

func (r *mreader) f32() float64 {
	return float64(math.Float32frombits(r.u32()))
}

func (r *mreader) str() string {
	n := int(r.u16())
	b := r.take(n)
	if b == nil {
		return ""
	}
	return string(b)
}

// count reads a length prefix and refuses one larger than the bytes remaining.
//
// Every length in this format is attacker-influenced once a model can arrive
// over the network. Bounding each against the bytes actually present turns a
// forged length from an out-of-memory kill into a decode error.
func (r *mreader) count(bytesPerItem int) int {
	n := int(r.u32())
	if r.err != nil {
		return 0
	}
	if n < 0 || (bytesPerItem > 0 && n > len(r.b)/bytesPerItem+1) {
		r.fail("length %d exceeds the %d bytes remaining", n, len(r.b))
		return 0
	}
	return n
}

// seal appends the CRC-32 and returns the finished container.
func seal(w *writer) []byte {
	return binary.LittleEndian.AppendUint32(w.b, crc32.ChecksumIEEE(w.b))
}

// openContainer verifies magic, version and checksum, and returns the payload
// reader together with the declared kind.
func openContainer(b []byte) (*mreader, ModelKind, error) {
	if len(b) < len(modelMagic)+2+2+4 {
		return nil, 0, fmt.Errorf("%w: %d bytes is shorter than a header", ErrModelFormat, len(b))
	}
	if string(b[:len(modelMagic)]) != modelMagic {
		return nil, 0, fmt.Errorf("%w: bad magic", ErrModelFormat)
	}
	body, want := b[:len(b)-4], binary.LittleEndian.Uint32(b[len(b)-4:])
	if got := crc32.ChecksumIEEE(body); got != want {
		return nil, 0, fmt.Errorf("%w: checksum %08x does not match the stored %08x", ErrModelFormat, got, want)
	}
	r := &mreader{b: body[len(modelMagic):]}
	if v := r.u16(); v != modelFormatVersion {
		return nil, 0, fmt.Errorf("%w: container version %d is not supported", ErrModelFormat, v)
	}
	kind := ModelKind(r.u16())
	return r, kind, r.err
}

func header(kind ModelKind) *writer {
	w := &writer{b: make([]byte, 0, 4096)}
	w.b = append(w.b, modelMagic...)
	w.u16(modelFormatVersion)
	w.u16(uint16(kind))
	return w
}

// ---------------------------------------------------------------------------
// GBT
// ---------------------------------------------------------------------------

// MarshalBinary encodes a float ensemble.
func (m *GBT) MarshalBinary() ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: nil model", ErrModelFormat)
	}
	w := header(KindGBT)
	w.f64(m.Base)
	w.f64(m.LearningRate)
	w.u32(uint32(m.NumFeatures))
	w.u32(uint32(len(m.Trees)))
	for i := range m.Trees {
		nodes := m.Trees[i].Nodes
		w.u32(uint32(len(nodes)))
		for _, n := range nodes {
			w.i32(n.Feature)
			w.i32(n.Left)
			w.i32(n.Right)
			w.f64(n.Threshold)
			w.f64(n.Value)
		}
	}
	return seal(w), nil
}

// UnmarshalBinary decodes a float ensemble.
func (m *GBT) UnmarshalBinary(b []byte) error {
	r, kind, err := openContainer(b)
	if err != nil {
		return err
	}
	if kind != KindGBT {
		return fmt.Errorf("%w: container holds a %s, not a gbt", ErrModelFormat, kind)
	}
	m.Base = r.f64()
	m.LearningRate = r.f64()
	m.NumFeatures = int(r.u32())
	nTrees := r.count(4)
	if r.err != nil {
		return r.err
	}
	m.Trees = make([]Tree, nTrees)
	for i := 0; i < nTrees; i++ {
		nNodes := r.count(28)
		if r.err != nil {
			return r.err
		}
		nodes := make([]TreeNode, nNodes)
		for j := range nodes {
			nodes[j].Feature = r.i32()
			nodes[j].Left = r.i32()
			nodes[j].Right = r.i32()
			nodes[j].Threshold = r.f64()
			nodes[j].Value = r.f64()
		}
		if r.err != nil {
			return r.err
		}
		if err := validateTree(nodes); err != nil {
			return err
		}
		m.Trees[i].Nodes = nodes
	}
	if r.err != nil {
		return r.err
	}
	if len(r.b) != 0 {
		return fmt.Errorf("%w: %d trailing bytes", ErrModelFormat, len(r.b))
	}
	return nil
}

// validateTree checks that every child index is in range and that the tree
// terminates.
//
// A decoded model runs an unchecked index in a tight loop; a corrupt child
// pointer that survived the CRC (because the file was truthfully encoded by a
// buggy producer) would either panic in the gateway's inference goroutine or
// spin forever. Checking once at load is worth the linear pass.
func validateTree(nodes []TreeNode) error {
	if len(nodes) == 0 {
		return fmt.Errorf("%w: empty tree", ErrModelFormat)
	}
	for i, n := range nodes {
		if n.Feature < 0 {
			continue
		}
		if int(n.Left) <= i || int(n.Left) >= len(nodes) || int(n.Right) <= i || int(n.Right) >= len(nodes) {
			// Requiring children to have a strictly greater index makes cycles
			// structurally impossible, which is stronger and cheaper than a
			// visited-set walk.
			return fmt.Errorf("%w: node %d has out-of-order children %d/%d", ErrModelFormat, i, n.Left, n.Right)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Quantised GBT
// ---------------------------------------------------------------------------

// MarshalBinary encodes the int8 ensemble. This is the artefact the SGU loads.
func (q *QuantisedGBT) MarshalBinary() ([]byte, error) {
	if q == nil {
		return nil, fmt.Errorf("%w: nil model", ErrModelFormat)
	}
	w := header(KindQuantisedGBT)
	w.f64(q.Base)
	w.f64(q.LeafScale)
	w.u32(uint32(q.NumFeatures))
	for i := 0; i < q.NumFeatures; i++ {
		w.f32(q.ThresholdZero[i])
		w.f32(q.ThresholdScale[i])
	}
	w.u32(uint32(len(q.Trees)))
	for i := range q.Trees {
		t := &q.Trees[i]
		w.u32(uint32(len(t.Feature)))
		for j := range t.Feature {
			w.u16(uint16(t.Feature[j]))
			w.u16(uint16(t.Left[j]))
			w.u16(uint16(t.Right[j]))
			w.u8(uint8(t.Threshold[j]))
			w.u8(uint8(t.Leaf[j]))
		}
	}
	return seal(w), nil
}

// UnmarshalBinary decodes the int8 ensemble.
func (q *QuantisedGBT) UnmarshalBinary(b []byte) error {
	r, kind, err := openContainer(b)
	if err != nil {
		return err
	}
	if kind != KindQuantisedGBT {
		return fmt.Errorf("%w: container holds a %s, not a gbt_int8", ErrModelFormat, kind)
	}
	q.Base = r.f64()
	q.LeafScale = r.f64()
	nf := r.count(8)
	if r.err != nil {
		return r.err
	}
	q.NumFeatures = nf
	q.ThresholdZero = make([]float64, nf)
	q.ThresholdScale = make([]float64, nf)
	for i := 0; i < nf; i++ {
		q.ThresholdZero[i] = r.f32()
		q.ThresholdScale[i] = r.f32()
	}
	nTrees := r.count(4)
	if r.err != nil {
		return r.err
	}
	q.Trees = make([]QuantisedTree, nTrees)
	for i := 0; i < nTrees; i++ {
		n := r.count(8)
		if r.err != nil {
			return r.err
		}
		t := QuantisedTree{
			Feature: make([]int16, n), Left: make([]int16, n), Right: make([]int16, n),
			Threshold: make([]int8, n), Leaf: make([]int8, n),
		}
		for j := 0; j < n; j++ {
			t.Feature[j] = int16(r.u16())
			t.Left[j] = int16(r.u16())
			t.Right[j] = int16(r.u16())
			t.Threshold[j] = int8(r.u8())
			t.Leaf[j] = int8(r.u8())
		}
		if r.err != nil {
			return r.err
		}
		for j := 0; j < n; j++ {
			if t.Feature[j] < 0 {
				continue
			}
			if int(t.Feature[j]) >= nf {
				return fmt.Errorf("%w: node %d splits on feature %d, outside the %d-wide input",
					ErrModelFormat, j, t.Feature[j], nf)
			}
			if int(t.Left[j]) <= j || int(t.Left[j]) >= n || int(t.Right[j]) <= j || int(t.Right[j]) >= n {
				return fmt.Errorf("%w: node %d has out-of-order children", ErrModelFormat, j)
			}
		}
		q.Trees[i] = t
	}
	if r.err != nil {
		return r.err
	}
	if len(r.b) != 0 {
		return fmt.Errorf("%w: %d trailing bytes", ErrModelFormat, len(r.b))
	}
	return nil
}

// ---------------------------------------------------------------------------
// LSTM
// ---------------------------------------------------------------------------

// MarshalBinary encodes the network at single precision.
func (n *LSTM) MarshalBinary() ([]byte, error) {
	if n == nil {
		return nil, fmt.Errorf("%w: nil model", ErrModelFormat)
	}
	w := header(KindLSTM)
	w.u32(uint32(n.InputSize))
	w.u32(uint32(n.Hidden))
	w.f32(n.By)
	w.f32(n.TargetMean)
	w.f32(n.TargetScale)
	writeF32Slice(w, n.Wx)
	writeF32Slice(w, n.Wh)
	writeF32Slice(w, n.B)
	writeF32Slice(w, n.Wy)
	writeF32Slice(w, n.InputMean)
	writeF32Slice(w, n.InputScale)
	return seal(w), nil
}

// UnmarshalBinary decodes the network and checks that the weight shapes agree
// with the declared dimensions.
func (n *LSTM) UnmarshalBinary(b []byte) error {
	r, kind, err := openContainer(b)
	if err != nil {
		return err
	}
	if kind != KindLSTM {
		return fmt.Errorf("%w: container holds a %s, not an lstm", ErrModelFormat, kind)
	}
	n.InputSize = int(r.u32())
	n.Hidden = int(r.u32())
	n.By = r.f32()
	n.TargetMean = r.f32()
	n.TargetScale = r.f32()
	n.Wx = readF32Slice(r)
	n.Wh = readF32Slice(r)
	n.B = readF32Slice(r)
	n.Wy = readF32Slice(r)
	n.InputMean = readF32Slice(r)
	n.InputScale = readF32Slice(r)
	if r.err != nil {
		return r.err
	}
	if n.InputSize <= 0 || n.Hidden <= 0 {
		return fmt.Errorf("%w: non-positive dimensions %dx%d", ErrModelFormat, n.InputSize, n.Hidden)
	}
	switch {
	case len(n.Wx) != 4*n.Hidden*n.InputSize,
		len(n.Wh) != 4*n.Hidden*n.Hidden,
		len(n.B) != 4*n.Hidden,
		len(n.Wy) != n.Hidden,
		len(n.InputMean) != n.InputSize,
		len(n.InputScale) != n.InputSize:
		return fmt.Errorf("%w: weight shapes disagree with dimensions %dx%d", ErrModelFormat, n.InputSize, n.Hidden)
	}
	for i, s := range n.InputScale {
		if s == 0 || math.IsNaN(s) {
			return fmt.Errorf("%w: input scale %d is %v, which would divide by zero at inference", ErrModelFormat, i, s)
		}
	}
	if len(r.b) != 0 {
		return fmt.Errorf("%w: %d trailing bytes", ErrModelFormat, len(r.b))
	}
	return nil
}

func writeF32Slice(w *writer, v []float64) {
	w.u32(uint32(len(v)))
	for _, x := range v {
		w.f32(x)
	}
}

func readF32Slice(r *mreader) []float64 {
	n := r.count(4)
	if r.err != nil {
		return nil
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = r.f32()
	}
	return out
}

// ---------------------------------------------------------------------------
// Isolation forest
// ---------------------------------------------------------------------------

// MarshalBinary encodes the forest.
func (f *IsolationForest) MarshalBinary() ([]byte, error) {
	if f == nil {
		return nil, fmt.Errorf("%w: nil model", ErrModelFormat)
	}
	w := header(KindIsoForest)
	w.u32(uint32(f.SampleSize))
	w.u32(uint32(f.NumFeatures))
	w.f64(f.Normaliser)
	w.u32(uint32(len(f.FeatureNames)))
	for _, n := range f.FeatureNames {
		w.str(n)
	}
	writeF32Slice(w, f.Median)
	writeF32Slice(w, f.MAD)
	w.u32(uint32(len(f.Trees)))
	for i := range f.Trees {
		nodes := f.Trees[i].Nodes
		w.u32(uint32(len(nodes)))
		for _, n := range nodes {
			w.i32(n.Feature)
			w.i32(n.Left)
			w.i32(n.Right)
			w.i32(n.Size)
			w.f32(n.Split)
		}
	}
	return seal(w), nil
}

// UnmarshalBinary decodes the forest.
func (f *IsolationForest) UnmarshalBinary(b []byte) error {
	r, kind, err := openContainer(b)
	if err != nil {
		return err
	}
	if kind != KindIsoForest {
		return fmt.Errorf("%w: container holds a %s, not an isolation_forest", ErrModelFormat, kind)
	}
	f.SampleSize = int(r.u32())
	f.NumFeatures = int(r.u32())
	f.Normaliser = r.f64()
	nNames := r.count(2)
	if r.err != nil {
		return r.err
	}
	f.FeatureNames = make([]string, nNames)
	for i := range f.FeatureNames {
		f.FeatureNames[i] = r.str()
	}
	f.Median = readF32Slice(r)
	f.MAD = readF32Slice(r)
	nTrees := r.count(4)
	if r.err != nil {
		return r.err
	}
	f.Trees = make([]isoTree, nTrees)
	for i := 0; i < nTrees; i++ {
		n := r.count(20)
		if r.err != nil {
			return r.err
		}
		nodes := make([]isoNode, n)
		for j := range nodes {
			nodes[j].Feature = r.i32()
			nodes[j].Left = r.i32()
			nodes[j].Right = r.i32()
			nodes[j].Size = r.i32()
			nodes[j].Split = r.f32()
		}
		if r.err != nil {
			return r.err
		}
		for j, nd := range nodes {
			if nd.Feature < 0 {
				continue
			}
			if int(nd.Left) <= j || int(nd.Left) >= n || int(nd.Right) <= j || int(nd.Right) >= n {
				return fmt.Errorf("%w: isolation node %d has out-of-order children", ErrModelFormat, j)
			}
		}
		f.Trees[i].Nodes = nodes
	}
	if r.err != nil {
		return r.err
	}
	if len(r.b) != 0 {
		return fmt.Errorf("%w: %d trailing bytes", ErrModelFormat, len(r.b))
	}
	return nil
}
