package domain_test

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/usslp/usslp/platform/internal/ota/domain"
)

// deflateForTest compresses with the same settings the patch format uses, so a
// hand-forged patch is byte-compatible with a real one.
func deflateForTest(t *testing.T, in []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := flate.NewWriter(&out, flate.BestCompression)
	if err != nil {
		t.Fatalf("compressor: %v", err)
	}
	if _, err := w.Write(in); err != nil {
		t.Fatalf("compress: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("finish compressing: %v", err)
	}
	return out.Bytes()
}

// roundTrip asserts that Apply(base, Diff(base, target)) == target, which is
// the only property of a patch format that actually matters.
func roundTrip(t *testing.T, name string, base, target []byte) *domain.Delta {
	t.Helper()
	d, err := domain.Diff(base, target)
	if err != nil {
		t.Fatalf("%s: diff: %v", name, err)
	}
	got, err := domain.Apply(base, d.Bytes)
	if err != nil {
		t.Fatalf("%s: apply: %v", name, err)
	}
	if !bytes.Equal(got, target) {
		t.Fatalf("%s: reconstructed image differs from the target (%d vs %d bytes)",
			name, len(got), len(target))
	}
	return d
}

func TestDeltaRoundTrip(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(1))
	randomBytes := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.Intn(256))
		}
		return b
	}
	repeat := func(pattern string, n int) []byte {
		return bytes.Repeat([]byte(pattern), n)
	}

	base := randomBytes(200_000)

	cases := []struct {
		name         string
		base, target []byte
	}{
		{"identical", base, append([]byte(nil), base...)},
		{"empty base", nil, randomBytes(5_000)},
		{"empty target", base, nil},
		{"both empty", nil, nil},
		{"target shorter than a block", base, []byte("short")},
		{"base shorter than a block", []byte("short"), randomBytes(5_000)},
		{"single byte inserted at the front", base, append([]byte{0x42}, base...)},
		{"single byte deleted from the front", base, base[1:]},
		{"byte changed in the middle", base, func() []byte {
			out := append([]byte(nil), base...)
			out[len(out)/2] ^= 0xff
			return out
		}()},
		{"region inserted in the middle", base, func() []byte {
			mid := len(base) / 2
			out := append([]byte(nil), base[:mid]...)
			out = append(out, randomBytes(9_000)...)
			return append(out, base[mid:]...)
		}()},
		{"region deleted from the middle", base, func() []byte {
			out := append([]byte(nil), base[:60_000]...)
			return append(out, base[90_000:]...)
		}()},
		{"blocks reordered", base, func() []byte {
			out := append([]byte(nil), base[100_000:]...)
			return append(out, base[:100_000]...)
		}()},
		// Pathological: nothing whatsoever in common. The matcher must not
		// blow up, and the patch must still apply correctly.
		{"nothing in common", base, randomBytes(180_000)},
		// Pathological: a base made entirely of one repeating pattern, so a
		// single hash bucket names thousands of offsets.
		{"degenerate repetition", repeat("A", 100_000), repeat("A", 90_000)},
		{"zero fill", make([]byte, 120_000), make([]byte, 130_000)},
		// Pathological: the target is the base repeated, so every window
		// matches in two places.
		{"target is the base twice", base, append(append([]byte(nil), base...), base...)},
		{"one byte", []byte{1}, []byte{2}},
	}

	for _, tc := range cases {
		d := roundTrip(t, tc.name, tc.base, tc.target)
		if d.TargetSize != len(tc.target) {
			t.Fatalf("%s: header declares %d bytes, target is %d", tc.name, d.TargetSize, len(tc.target))
		}
	}
}

func TestDeltaOfAnIdenticalImageIsTiny(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(2))
	image := make([]byte, 300_000)
	for i := range image {
		image[i] = byte(rng.Intn(256))
	}
	d := roundTrip(t, "identical", image, image)
	// One copy instruction over the whole image, compressed. A hundred bytes is
	// generous; the point is that it is not proportional to the image.
	if len(d.Bytes) > 200 {
		t.Fatalf("patch for an unchanged image is %d bytes, want a constant-size one", len(d.Bytes))
	}
	if d.LiteralBytes != 0 {
		t.Fatalf("patch for an unchanged image carries %d literal bytes", d.LiteralBytes)
	}
}

func TestApplyRefusesTheWrongBase(t *testing.T) {
	t.Parallel()
	base := bytes.Repeat([]byte("firmware"), 10_000)
	target := append(append([]byte(nil), base...), []byte("new code")...)
	d, err := domain.Diff(base, target)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	wrong := bytes.Repeat([]byte("different"), 10_000)
	if _, err := domain.Apply(wrong, d.Bytes); !errors.Is(err, domain.ErrDeltaBaseMismatch) {
		t.Fatalf("error = %v, want ErrDeltaBaseMismatch; applying a patch to the wrong base "+
			"would produce an untested image on a device that has to be retrieved by hand", err)
	}
}

func TestApplyRefusesTamperedAndMalformedPatches(t *testing.T) {
	t.Parallel()
	base := bytes.Repeat([]byte("firmware-image-"), 20_000)
	target := append(append([]byte(nil), base[:100_000]...), []byte("patched region")...)
	d, err := domain.Diff(base, target)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}

	t.Run("truncated", func(t *testing.T) {
		if _, err := domain.Apply(base, d.Bytes[:len(d.Bytes)/2]); err == nil {
			t.Fatal("a truncated patch was accepted")
		}
	})
	t.Run("bad magic", func(t *testing.T) {
		bad := append([]byte(nil), d.Bytes...)
		bad[0] = 'X'
		if _, err := domain.Apply(base, bad); !errors.Is(err, domain.ErrDeltaFormat) {
			t.Fatalf("error = %v, want ErrDeltaFormat", err)
		}
	})
	t.Run("corrupted body", func(t *testing.T) {
		bad := append([]byte(nil), d.Bytes...)
		// Flip a bit deep in the compressed instruction stream. It either fails
		// to decompress, fails a bound check, or reconstructs something whose
		// digest is wrong — all three are refusals, which is the point.
		bad[len(bad)-8] ^= 0x01
		if _, err := domain.Apply(base, bad); err == nil {
			t.Fatal("a corrupted patch produced an image; the digest check must catch it")
		}
	})
	t.Run("copy beyond the base", func(t *testing.T) {
		// A hand-built patch whose copy instruction reaches past the end of the
		// base. It is the shape an attacker would use to read adjacent memory on
		// a device, and the bound check must refuse it before the digest does.
		forged := forgeOverlongCopy(t, base, target)
		if _, err := domain.Apply(base, forged); !errors.Is(err, domain.ErrDeltaFormat) {
			t.Fatalf("error = %v, want ErrDeltaFormat for an out-of-range copy", err)
		}
	})
}

// forgeOverlongCopy builds a syntactically valid patch whose single copy
// instruction runs past the end of the base image.
func forgeOverlongCopy(t *testing.T, base, target []byte) []byte {
	t.Helper()
	var ops bytes.Buffer
	var scratch [binary.MaxVarintLen64]byte
	ops.WriteByte(1) // opCopy
	n := binary.PutUvarint(scratch[:], uint64(len(base)+1024))
	ops.Write(scratch[:n])
	n = binary.PutUvarint(scratch[:], 0)
	ops.Write(scratch[:n])

	compressed := deflateForTest(t, ops.Bytes())
	baseSum := sha256.Sum256(base)
	targetSum := sha256.Sum256(target)

	var out bytes.Buffer
	out.WriteString("USDELTA1")
	out.Write(baseSum[:])
	out.Write(targetSum[:])
	n = binary.PutUvarint(scratch[:], uint64(len(base)+1024))
	out.Write(scratch[:n])
	n = binary.PutUvarint(scratch[:], uint64(ops.Len()))
	out.Write(scratch[:n])
	out.Write(compressed)
	return out.Bytes()
}

// ---------------------------------------------------------------------------
// Compression ratio on a realistic firmware pair
// ---------------------------------------------------------------------------

// buildFirmwarePair synthesises a base firmware image and a point release of
// it, with the internal structure and the churn a real pair has.
//
// The structure matters for the measurement. Real firmware is not random bytes:
// it is mostly compiled machine code drawn from a small instruction vocabulary,
// a string table that compresses extremely well, a high-entropy resource blob
// (an E-Ink waveform table) that does not change between point releases, and a
// padded tail. A delta measured against random data would be meaningless in
// both directions — the matcher would find nothing and the compressor would
// achieve nothing.
//
// The churn matters just as much. A point release is not "the same image plus
// a function": recompiling shifts register allocation and inlining decisions
// across the translation units that changed, the linker moves everything after
// an insertion, and the vector table's absolute addresses all move with it.
// The pair below models all four: a version string change, twelve new functions
// inserted mid-image, roughly eight percent of existing functions regenerated
// with different bytes, and a vector table whose every entry shifts. Without
// that churn a synthetic measurement flatters the algorithm by an order of
// magnitude.
func buildFirmwarePair() (base, target []byte) {
	const (
		functionsBeforeInsert = 900
		functionsAfterInsert  = 400
		insertedFunctions     = 12
		churnEveryNth         = 12 // roughly 8% of functions are recompiled
	)
	opcodes := [][]byte{
		{0xb5, 0x80}, {0xaf, 0x00}, {0x68, 0x7b}, {0x60, 0x3b},
		{0x1c, 0x18}, {0xf7, 0xff}, {0xbd, 0x80}, {0x46, 0xc0},
		{0x4b, 0x0c}, {0x22, 0x01}, {0x70, 0x1a}, {0xe7, 0xfe},
	}
	// A function's bytes are a deterministic function of its own seed, so the
	// two builds agree on every function neither of them recompiled.
	function := func(seed int64) []byte {
		r := rand.New(rand.NewSource(seed))
		n := 40 + r.Intn(60)
		out := make([]byte, 0, n*2)
		for i := 0; i < n; i++ {
			out = append(out, opcodes[r.Intn(len(opcodes))]...)
		}
		return out
	}
	strings := []string{
		"battery low", "mesh joined", "mesh left", "price updated",
		"attestation failed", "ota begin", "ota complete", "ota failed",
		"display refresh", "partial refresh", "nfc tap", "tamper detected",
	}
	waveRng := rand.New(rand.NewSource(0x5A4E))
	wave := make([]byte, 24_000)
	for i := range wave {
		wave[i] = byte(waveRng.Intn(256))
	}

	build := func(version string, isRelease bool) []byte {
		var out bytes.Buffer
		out.WriteString("USSLP-ESL-FW\x00")
		out.WriteString(version)
		out.WriteByte(0)
		fmt.Fprintf(&out, "build=%08x\x00", 0xC0FFEE)

		// Vector table: absolute addresses into the code section. Inserting
		// functions moves the code, so every entry differs between builds.
		shift := 0
		if isRelease {
			shift = insertedFunctions * 0x40
		}
		for i := 0; i < 64; i++ {
			var p [4]byte
			binary.LittleEndian.PutUint32(p[:], uint32(0x08001000+i*0x40+shift))
			out.Write(p[:])
		}

		for i := 0; i < functionsBeforeInsert; i++ {
			seed := int64(i)
			if isRelease && i%churnEveryNth == 0 {
				seed = int64(i) | 1<<40 // recompiled: different bytes
			}
			out.Write(function(seed))
		}
		if isRelease {
			for i := 0; i < insertedFunctions; i++ {
				out.Write(function(int64(i) | 1<<41))
			}
		}
		for i := 0; i < functionsAfterInsert; i++ {
			seed := int64(i) | 1<<32
			if isRelease && i%churnEveryNth == 0 {
				seed |= 1 << 40
			}
			out.Write(function(seed))
		}

		for i := 0; i < 300; i++ {
			s := strings[i%len(strings)]
			if isRelease && i%37 == 0 {
				s = "ota verify " + s
			}
			out.WriteString(s)
			out.WriteByte(0)
		}
		out.Write(wave)
		for out.Len()%4096 != 0 {
			out.WriteByte(0xff)
		}
		return out.Bytes()
	}
	return build("1.4.2", false), build("1.4.3", true)
}

func TestDeltaCompressionOnRealisticFirmware(t *testing.T) {
	t.Parallel()
	// A realistic point release: a version bump, new functions inserted
	// mid-image so everything after shifts, roughly eight percent of existing
	// functions recompiled, a shifted vector table and changed log strings.
	base, target := buildFirmwarePair()

	if len(base) < 100_000 {
		t.Fatalf("synthetic firmware is %d bytes, too small to measure anything", len(base))
	}
	d := roundTrip(t, "firmware point release", base, target)
	ship, deltaBytes, fullBytes := domain.ShouldShipDelta(d, target)

	savings := 1 - float64(deltaBytes)/float64(fullBytes)
	t.Logf("firmware delta measurement: base=%d target=%d patch=%d compressed-full=%d "+
		"patch/target=%.1f%% patch/compressed-full=%.1f%% savings-vs-compressed=%.1f%% "+
		"copies=%d literals=%d literal-bytes=%d",
		len(base), len(target), deltaBytes, fullBytes,
		d.Ratio()*100, float64(deltaBytes)/float64(fullBytes)*100, savings*100,
		d.Copies, d.Literals, d.LiteralBytes)

	if !ship {
		t.Fatalf("a point release produced a patch of %d bytes against a %d-byte compressed image; "+
			"the matcher is not finding the unchanged regions", deltaBytes, fullBytes)
	}
	// The blueprint claims 60-80% payload reduction. The honest comparison is
	// against the compressed image, since a full rollout would compress it too.
	if savings < 0.60 {
		t.Fatalf("delta saves %.1f%% against the compressed image, below the 60%% the blueprint claims",
			savings*100)
	}
	// And against the raw image, which is what a naive rollout would send.
	if d.Savings() < 0.80 {
		t.Fatalf("delta is %.1f%% of the raw image, want under 20%%", d.Ratio()*100)
	}
}

func TestShouldShipDeltaRefusesAWorthlessPatch(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(9))
	random := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.Intn(256))
		}
		return b
	}
	// A from-scratch rewrite: nothing in common. The patch is necessarily
	// larger than the compressed image, and shipping it would cost a
	// battery-powered fleet more than the image it replaces.
	base, target := random(150_000), random(150_000)
	d, err := domain.Diff(base, target)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	ship, deltaBytes, fullBytes := domain.ShouldShipDelta(d, target)
	if ship {
		t.Fatalf("a patch of %d bytes was preferred over a %d-byte compressed image", deltaBytes, fullBytes)
	}
}

func BenchmarkDeltaDiff(b *testing.B) {
	base, target := buildFirmwarePair()
	b.SetBytes(int64(len(target)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := domain.Diff(base, target); err != nil {
			b.Fatal(err)
		}
	}
}
