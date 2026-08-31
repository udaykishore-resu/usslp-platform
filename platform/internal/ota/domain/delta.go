package domain

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
)

// ---------------------------------------------------------------------------
// Binary delta encoding
//
// # Why a delta at all
//
// A firmware image for a Tier 1 label is a few hundred kilobytes. Pushing one
// to a label means pushing it across a Zigbee mesh that is also carrying price
// updates, at a data rate measured in tens of kilobits per second, to a device
// whose entire seven-year energy budget is a coin cell. The radio is the single
// most expensive thing a label ever does: transmitting a byte costs orders of
// magnitude more energy than storing one. A rollout that moves 20% of the bytes
// does not merely finish five times sooner — it costs a fifth of the battery,
// and battery is the resource the product is sold on.
//
// # Why this algorithm
//
// This is a content-defined block matcher with a rolling hash, in the rsync
// lineage, with byte-level match extension in both directions and a final
// entropy-coding pass over the whole instruction stream.
//
// The rolling hash is what makes it work on firmware specifically. Two builds
// of the same image differ by a handful of edits — a version string, a patched
// function, a changed constant — but an insertion of nine bytes near the front
// shifts everything after it, so a fixed-block comparison finds nothing in
// common past the first edit. Rolling the window one byte at a time over the
// target and asking "does the base contain this block anywhere" recovers the
// alignment immediately after each edit, which is exactly the structure of a
// firmware diff.
//
// Match extension is what makes the matches long. A confirmed block match is
// extended backwards over the literals already accumulated and forwards to the
// end of the agreement, so a 200 KiB unchanged region becomes one copy
// instruction of 200 KiB rather than 3,200 block-sized ones. Without it the
// instruction overhead alone would eat much of the win.
//
// The final flate pass matters more than it looks. What survives the matcher is
// the genuinely new material — new code, new strings — which is ordinary
// compressible data, and compressing it typically halves it again.
//
// # What was not used and why
//
// A suffix-array construction in the bsdiff style finds better matches on
// pathological inputs and is the right answer when patch size is the only
// consideration. It is not used here because it costs O(n log n) time and
// several times the image size in memory per patch, and the platform generates
// a patch per (from-version, to-version, hardware tier) pair on a rollout
// planner that also has to answer HTTP requests. The measured difference on
// real firmware pairs does not pay for that, and a patch format is a thing you
// live with for years.
// ---------------------------------------------------------------------------

// deltaMagic identifies the format and its version. A patch is applied by
// firmware that may be older than the tool that produced it, so the version is
// checked before anything else is read.
var deltaMagic = [8]byte{'U', 'S', 'D', 'E', 'L', 'T', 'A', '1'}

// Block sizing. The window is the unit the rolling hash indexes.
//
// Sixty-four bytes is small enough to find the short unchanged runs between
// adjacent edits in a patched function, and large enough that the index over a
// 512 KiB base is a few thousand entries rather than half a million. Below
// about 32 bytes the index costs more than the matches it finds; above about
// 256 it misses everything between two nearby edits.
const (
	deltaBlockSize = 64
	// minMatch is the shortest match worth emitting as a copy instruction. A
	// copy costs about five bytes of instruction, so anything shorter is
	// cheaper as a literal.
	minMatch = 16
)

// Instruction opcodes.
const (
	opCopy    = 1 // copy N bytes from the base at offset O
	opLiteral = 2 // insert N literal bytes from the patch
)

// Errors returned by the delta codec.
var (
	// ErrDeltaFormat means the patch is not a USSLP delta or is truncated.
	ErrDeltaFormat = errors.New("ota: malformed delta")
	// ErrDeltaBaseMismatch means the patch was computed against a different
	// base image than the one supplied. Applying it anyway would produce a
	// plausible-looking image that has never been tested, on a device that has
	// to be retrieved by hand if it does not boot.
	ErrDeltaBaseMismatch = errors.New("ota: delta does not apply to this base image")
	// ErrDeltaResultMismatch means the reconstructed image does not hash to the
	// target the patch claims. It is the last line of defence against a
	// corrupted transfer and against a patch built to reference memory outside
	// the base.
	ErrDeltaResultMismatch = errors.New("ota: patched image does not match the expected digest")
)

// Delta is an encoded binary patch together with the digests that bind it to
// exactly one base and one target.
type Delta struct {
	// Bytes is the wire form: header plus the compressed instruction stream.
	Bytes []byte
	// BaseSHA256 and TargetSHA256 are the images this patch turns into each
	// other.
	BaseSHA256   [32]byte
	TargetSHA256 [32]byte
	// TargetSize is the length of the reconstructed image, used to pre-allocate
	// on the device and to reject a patch that would expand without bound.
	TargetSize int
	// Copies and Literals count the instructions, which is what a rollout
	// planner looks at when a delta comes out larger than expected.
	Copies   int
	Literals int
	// LiteralBytes is how much genuinely new material the patch carries, before
	// compression. It is the honest measure of how different two builds are.
	LiteralBytes int
}

// Ratio returns the patch size as a fraction of the full image. A ratio of 0.2
// means the rollout moves a fifth of the bytes.
func (d *Delta) Ratio() float64 {
	if d.TargetSize == 0 {
		return 0
	}
	return float64(len(d.Bytes)) / float64(d.TargetSize)
}

// Savings returns the fraction of payload the patch avoids sending.
func (d *Delta) Savings() float64 {
	r := d.Ratio()
	if r >= 1 {
		return 0
	}
	return 1 - r
}

// Diff computes a patch that turns base into target.
//
// It never fails on the content of its inputs: an empty base, an empty target,
// identical images and completely unrelated images are all valid and all
// produce a correct patch. The pathological case — two images with nothing in
// common — produces a patch slightly larger than the compressed target, which
// is why [ShouldShipDelta] exists: the rollout planner compares the two and
// ships whichever is smaller.
func Diff(base, target []byte) (*Delta, error) {
	d := &Delta{
		BaseSHA256:   sha256.Sum256(base),
		TargetSHA256: sha256.Sum256(target),
		TargetSize:   len(target),
	}

	index := buildBlockIndex(base)

	var ops bytes.Buffer
	var literal []byte
	// pos walks the target. Everything before pos has been emitted.
	pos := 0
	emitLiteral := func() {
		if len(literal) == 0 {
			return
		}
		writeOp(&ops, opLiteral, uint64(len(literal)), 0)
		ops.Write(literal)
		d.Literals++
		d.LiteralBytes += len(literal)
		literal = literal[:0]
	}

	if len(base) >= deltaBlockSize && len(target) >= deltaBlockSize {
		roll := newRollingHash(target[:deltaBlockSize])
		for pos <= len(target)-deltaBlockSize {
			matchOff, matchLen := index.find(base, target, pos, roll.sum())
			if matchLen >= minMatch {
				// Extend the match backwards over literals already accumulated:
				// the rolling window found the *start* of an aligned block, but
				// the agreement often begins several bytes earlier.
				back := 0
				for back < len(literal) && matchOff-back > 0 &&
					base[matchOff-back-1] == target[pos-back-1] {
					back++
				}
				literal = literal[:len(literal)-back]
				emitLiteral()

				writeOp(&ops, opCopy, uint64(matchLen+back), uint64(matchOff-back))
				d.Copies++
				pos += matchLen
				if pos > len(target)-deltaBlockSize {
					break
				}
				roll = newRollingHash(target[pos : pos+deltaBlockSize])
				continue
			}
			literal = append(literal, target[pos])
			pos++
			if pos <= len(target)-deltaBlockSize {
				roll.roll(target[pos-1], target[pos+deltaBlockSize-1])
			}
		}
	}
	if pos < len(target) {
		literal = append(literal, target[pos:]...)
	}
	emitLiteral()

	body, err := deflate(ops.Bytes())
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	out.Write(deltaMagic[:])
	out.Write(d.BaseSHA256[:])
	out.Write(d.TargetSHA256[:])
	var scratch [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(scratch[:], uint64(len(target)))
	out.Write(scratch[:n])
	n = binary.PutUvarint(scratch[:], uint64(ops.Len()))
	out.Write(scratch[:n])
	out.Write(body)
	d.Bytes = out.Bytes()
	return d, nil
}

// Apply reconstructs the target image from a base and a patch.
//
// Every bound is checked. This function runs on the update path of a device
// that has to be physically retrieved if it does not boot, so a patch that
// claims to copy from beyond the end of the base, or to produce more bytes than
// its header declares, is refused rather than clamped. The final digest check
// is what makes the whole pipeline safe: whatever happened in transit, the
// image that gets flashed is the image the signature was computed over or none
// at all.
func Apply(base, delta []byte) ([]byte, error) {
	if len(delta) < len(deltaMagic)+64+2 {
		return nil, fmt.Errorf("%w: patch is %d bytes, shorter than a header", ErrDeltaFormat, len(delta))
	}
	if !bytes.Equal(delta[:len(deltaMagic)], deltaMagic[:]) {
		return nil, fmt.Errorf("%w: bad magic", ErrDeltaFormat)
	}
	r := bytes.NewReader(delta[len(deltaMagic):])

	var wantBase, wantTarget [32]byte
	if _, err := io.ReadFull(r, wantBase[:]); err != nil {
		return nil, fmt.Errorf("%w: truncated base digest", ErrDeltaFormat)
	}
	if _, err := io.ReadFull(r, wantTarget[:]); err != nil {
		return nil, fmt.Errorf("%w: truncated target digest", ErrDeltaFormat)
	}
	if got := sha256.Sum256(base); got != wantBase {
		return nil, fmt.Errorf("%w: patch expects base %x, got %x",
			ErrDeltaBaseMismatch, wantBase[:8], got[:8])
	}
	targetSize, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated target size", ErrDeltaFormat)
	}
	opsLen, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated instruction length", ErrDeltaFormat)
	}
	// A declared size beyond any plausible firmware image is refused before a
	// single byte is allocated: the header is attacker-reachable on a device
	// with a few hundred kilobytes of RAM.
	const maxImage = 256 << 20
	if targetSize > maxImage || opsLen > maxImage {
		return nil, fmt.Errorf("%w: declares a %d-byte image", ErrDeltaFormat, targetSize)
	}

	compressed := make([]byte, r.Len())
	if _, err := io.ReadFull(r, compressed); err != nil {
		return nil, fmt.Errorf("%w: truncated body", ErrDeltaFormat)
	}
	ops, err := inflate(compressed, int(opsLen))
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, targetSize)
	cursor := bytes.NewReader(ops)
	for cursor.Len() > 0 {
		op, err := cursor.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("%w: truncated opcode", ErrDeltaFormat)
		}
		length, err := binary.ReadUvarint(cursor)
		if err != nil {
			return nil, fmt.Errorf("%w: truncated length", ErrDeltaFormat)
		}
		if uint64(len(out))+length > targetSize {
			return nil, fmt.Errorf("%w: instruction would produce %d bytes, header declares %d",
				ErrDeltaFormat, uint64(len(out))+length, targetSize)
		}
		switch op {
		case opCopy:
			offset, err := binary.ReadUvarint(cursor)
			if err != nil {
				return nil, fmt.Errorf("%w: truncated copy offset", ErrDeltaFormat)
			}
			if offset+length > uint64(len(base)) {
				return nil, fmt.Errorf("%w: copy of %d bytes at offset %d exceeds the %d-byte base",
					ErrDeltaFormat, length, offset, len(base))
			}
			out = append(out, base[offset:offset+length]...)
		case opLiteral:
			if uint64(cursor.Len()) < length {
				return nil, fmt.Errorf("%w: literal of %d bytes with %d remaining",
					ErrDeltaFormat, length, cursor.Len())
			}
			start := len(ops) - cursor.Len()
			out = append(out, ops[start:start+int(length)]...)
			if _, err := cursor.Seek(int64(length), io.SeekCurrent); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrDeltaFormat, err)
			}
		default:
			return nil, fmt.Errorf("%w: unknown opcode %d", ErrDeltaFormat, op)
		}
	}
	if uint64(len(out)) != targetSize {
		return nil, fmt.Errorf("%w: reconstructed %d bytes, header declares %d",
			ErrDeltaFormat, len(out), targetSize)
	}
	if got := sha256.Sum256(out); got != wantTarget {
		return nil, fmt.Errorf("%w: got %x, want %x", ErrDeltaResultMismatch, got[:8], wantTarget[:8])
	}
	return out, nil
}

// ShouldShipDelta reports whether a patch is worth sending in preference to the
// whole image, and by how much.
//
// The comparison is against the *compressed* full image, not the raw one.
// Comparing a compressed patch with an uncompressed image would make every
// delta look like a win, including the ones that are not, and the rollout that
// discovers this is the one where a from-scratch rewrite of the firmware ships
// as a patch that is larger than the thing it replaces.
func ShouldShipDelta(d *Delta, target []byte) (ship bool, deltaBytes, fullBytes int) {
	full, err := deflate(target)
	if err != nil {
		// Compression of a byte slice cannot fail in practice; if it somehow
		// does, the safe answer is to ship the image rather than a patch whose
		// benefit could not be established.
		return false, len(d.Bytes), len(target)
	}
	return len(d.Bytes) < len(full), len(d.Bytes), len(full)
}

// writeOp appends one instruction to the stream.
func writeOp(w *bytes.Buffer, op byte, length, offset uint64) {
	var scratch [binary.MaxVarintLen64]byte
	w.WriteByte(op)
	n := binary.PutUvarint(scratch[:], length)
	w.Write(scratch[:n])
	if op == opCopy {
		n = binary.PutUvarint(scratch[:], offset)
		w.Write(scratch[:n])
	}
}

// blockIndex maps a rolling-hash value to every base offset whose window hashes
// to it.
type blockIndex struct {
	buckets map[uint32][]int32
	strong  map[int32]uint64
}

// maxCandidatesPerBucket bounds the work spent on one hash value.
//
// Firmware is full of repeated content — padding runs, zero-filled sections,
// repeated jump tables — and a single hash value can name thousands of offsets.
// Checking all of them turns the matcher quadratic on exactly the images it is
// meant to be fast on. Sixteen is enough to find a genuine match while keeping
// the worst case linear; the cost of missing one is a slightly larger patch,
// never a wrong one.
const maxCandidatesPerBucket = 16

func buildBlockIndex(base []byte) *blockIndex {
	idx := &blockIndex{
		buckets: make(map[uint32][]int32),
		strong:  make(map[int32]uint64),
	}
	if len(base) < deltaBlockSize {
		return idx
	}
	// The base is indexed at block granularity rather than at every offset: a
	// match anywhere inside an aligned block is found because the *target* is
	// rolled one byte at a time, so only one side needs a dense index.
	for off := 0; off+deltaBlockSize <= len(base); off += deltaBlockSize {
		window := base[off : off+deltaBlockSize]
		weak := weakHash(window)
		if len(idx.buckets[weak]) < maxCandidatesPerBucket {
			idx.buckets[weak] = append(idx.buckets[weak], int32(off))
			idx.strong[int32(off)] = strongHash(window)
		}
	}
	return idx
}

// find looks for the target window at pos in the base and returns the offset
// and length of the longest forward match, or (0, 0).
func (idx *blockIndex) find(base, target []byte, pos int, weak uint32) (int, int) {
	candidates := idx.buckets[weak]
	if len(candidates) == 0 {
		return 0, 0
	}
	window := target[pos : pos+deltaBlockSize]
	// The strong hash is what turns a weak-hash coincidence into a real match.
	// Skipping it would produce patches that apply cleanly and reconstruct the
	// wrong image, caught only by the final digest — which is exactly the
	// failure that costs a truck roll.
	strong := strongHash(window)
	bestOff, bestLen := 0, 0
	for _, c := range candidates {
		off := int(c)
		if idx.strong[c] != strong {
			continue
		}
		if !bytes.Equal(base[off:off+deltaBlockSize], window) {
			continue
		}
		n := deltaBlockSize
		for off+n < len(base) && pos+n < len(target) && base[off+n] == target[pos+n] {
			n++
		}
		if n > bestLen {
			bestOff, bestLen = off, n
		}
	}
	return bestOff, bestLen
}

// rollingHash is the Adler-style two-part sum that lets the window advance one
// byte at a time in constant time.
type rollingHash struct {
	a, b uint32
	n    uint32
}

func newRollingHash(window []byte) *rollingHash {
	r := &rollingHash{n: uint32(len(window))}
	for i, c := range window {
		r.a += uint32(c)
		r.b += uint32(len(window)-i) * uint32(c)
	}
	return r
}

func (r *rollingHash) roll(out, in byte) {
	r.a = r.a - uint32(out) + uint32(in)
	r.b = r.b - r.n*uint32(out) + r.a
}

func (r *rollingHash) sum() uint32 { return (r.b << 16) | (r.a & 0xffff) }

// weakHash computes the rolling hash of a window from scratch.
func weakHash(window []byte) uint32 { return newRollingHash(window).sum() }

// strongHash is a 64-bit FNV-1a over the window. It is a collision check, not a
// security boundary — the security boundary is the Ed25519 signature over the
// image and the SHA-256 check in Apply — so a fast non-cryptographic hash is
// the right tool.
func strongHash(window []byte) uint64 {
	h := fnv.New64a()
	h.Write(window)
	return h.Sum64()
}

// deflate compresses with the standard library's DEFLATE at best compression.
func deflate(in []byte) ([]byte, error) {
	var out bytes.Buffer
	w, err := flate.NewWriter(&out, flate.BestCompression)
	if err != nil {
		return nil, fmt.Errorf("ota: init compressor: %w", err)
	}
	if _, err := w.Write(in); err != nil {
		return nil, fmt.Errorf("ota: compress delta: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("ota: finish compressing delta: %w", err)
	}
	return out.Bytes(), nil
}

// inflate decompresses, refusing to produce more than the declared size.
func inflate(in []byte, want int) ([]byte, error) {
	r := flate.NewReader(bytes.NewReader(in))
	defer r.Close()
	out := make([]byte, want)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("%w: decompress: %v", ErrDeltaFormat, err)
	}
	// Anything left over means the stream declares less than it carries, which
	// is a malformed patch rather than a harmless surplus.
	if n, _ := io.Copy(io.Discard, r); n > 0 {
		return nil, fmt.Errorf("%w: instruction stream is longer than its declared length", ErrDeltaFormat)
	}
	return out, nil
}
