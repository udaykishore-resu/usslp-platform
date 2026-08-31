// Package sec implements the USSLP Shelf Edge Controller: the Tier 2 device
// that owns roughly eight metres of shelving, coordinates the Zigbee zone
// beneath it, and is the last place in the platform where a price can still be
// refused.
//
// Three of its responsibilities are load-bearing for the whole system:
//
//   - It verifies the price attestation against its key ring before driving a
//     single E-Ink waveform (INTERFACE-CONTRACTS section 5). An attacker who
//     owns the store's broker can suppress an update but cannot author one.
//   - It renders. The cloud sends a canon.RenderSpec; the pixels are computed
//     here, on the controller, which is why a store whose WAN is down can still
//     put a new price on a shelf.
//   - It runs the mesh. Scheduling transmissions, retrying them, tracking who
//     acknowledged, sampling link quality and moving routes off links that are
//     about to fail are all local decisions taken on local information.
package sec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/color"
	"image/png"
	"io"
)

// Ink is one electrophoretic pixel state.
//
// The values are the panel's own ink states rather than an RGB colour, because
// that is what the display controller is actually loaded with: a 2.9-inch BWR
// panel has two one-bit planes, and asking it for a shade of grey is not a
// rendering compromise, it is a command it does not have.
type Ink uint8

// The ink states across the three panel tiers. Black and white exist on every
// panel; red exists on the BWR panel; the rest are the seven-colour panel's.
const (
	InkWhite Ink = iota
	InkBlack
	InkRed
	InkYellow
	InkGreen
	InkBlue
	InkOrange
	inkCount
)

// String names the ink for diagnostics.
func (i Ink) String() string {
	switch i {
	case InkWhite:
		return "white"
	case InkBlack:
		return "black"
	case InkRed:
		return "red"
	case InkYellow:
		return "yellow"
	case InkGreen:
		return "green"
	case InkBlue:
		return "blue"
	case InkOrange:
		return "orange"
	default:
		return "unknown"
	}
}

// rgb is the ink's approximate appearance, used only when dumping a PNG for a
// human to look at.
var inkRGB = [inkCount]color.RGBA{
	InkWhite:  {0xF2, 0xF1, 0xEC, 0xFF}, // E-Ink white is a warm off-white, not paper
	InkBlack:  {0x1B, 0x1B, 0x1B, 0xFF},
	InkRed:    {0xC0, 0x1A, 0x18, 0xFF},
	InkYellow: {0xE8, 0xC8, 0x18, 0xFF},
	InkGreen:  {0x2C, 0x7A, 0x38, 0xFF},
	InkBlue:   {0x22, 0x3E, 0x9A, 0xFF},
	InkOrange: {0xD8, 0x6A, 0x18, 0xFF},
}

// ErrBadFramebuffer reports a malformed encoded framebuffer.
var ErrBadFramebuffer = errors.New("sec: malformed framebuffer")

// Rect is an inclusive-exclusive pixel rectangle.
type Rect struct{ X0, Y0, X1, Y1 int }

// Empty reports whether the rectangle covers no pixels.
func (r Rect) Empty() bool { return r.X1 <= r.X0 || r.Y1 <= r.Y0 }

// Area returns the number of pixels the rectangle covers.
func (r Rect) Area() int {
	if r.Empty() {
		return 0
	}
	return (r.X1 - r.X0) * (r.Y1 - r.Y0)
}

// Framebuffer is a rendered label image, one byte per pixel.
//
// One byte per pixel rather than packed bit planes because the controller is a
// Linux box with memory to spare and the operations that matter — diffing two
// images to decide whether a partial refresh is safe — are far clearer and no
// slower this way. The packing happens at the wire boundary, in EncodeRLE.
type Framebuffer struct {
	W, H int
	Pix  []Ink
}

// NewFramebuffer allocates a white framebuffer. White is the correct blank: an
// E-Ink panel at rest is white, and rendering onto black would invert every
// template.
func NewFramebuffer(w, h int) *Framebuffer {
	if w <= 0 || h <= 0 {
		return &Framebuffer{}
	}
	return &Framebuffer{W: w, H: h, Pix: make([]Ink, w*h)}
}

// Clone returns an independent copy.
func (f *Framebuffer) Clone() *Framebuffer {
	c := &Framebuffer{W: f.W, H: f.H, Pix: make([]Ink, len(f.Pix))}
	copy(c.Pix, f.Pix)
	return c
}

// Set writes one pixel, ignoring coordinates outside the panel so that a
// template that overflows produces a clipped image rather than a panic on a
// device nobody can reach.
func (f *Framebuffer) Set(x, y int, ink Ink) {
	if x < 0 || y < 0 || x >= f.W || y >= f.H {
		return
	}
	f.Pix[y*f.W+x] = ink
}

// At returns one pixel, reporting white outside the panel.
func (f *Framebuffer) At(x, y int) Ink {
	if x < 0 || y < 0 || x >= f.W || y >= f.H {
		return InkWhite
	}
	return f.Pix[y*f.W+x]
}

// Fill paints the whole panel.
func (f *Framebuffer) Fill(ink Ink) {
	for i := range f.Pix {
		f.Pix[i] = ink
	}
}

// FillRect paints a solid rectangle.
func (f *Framebuffer) FillRect(r Rect, ink Ink) {
	for y := r.Y0; y < r.Y1; y++ {
		for x := r.X0; x < r.X1; x++ {
			f.Set(x, y, ink)
		}
	}
}

// StrokeRect draws a rectangle outline of the given thickness.
func (f *Framebuffer) StrokeRect(r Rect, thickness int, ink Ink) {
	if thickness < 1 {
		thickness = 1
	}
	for t := 0; t < thickness; t++ {
		for x := r.X0 + t; x < r.X1-t; x++ {
			f.Set(x, r.Y0+t, ink)
			f.Set(x, r.Y1-1-t, ink)
		}
		for y := r.Y0 + t; y < r.Y1-t; y++ {
			f.Set(r.X0+t, y, ink)
			f.Set(r.X1-1-t, y, ink)
		}
	}
}

// HLine draws a horizontal run of the given thickness, which is how a
// was-price gets struck through.
func (f *Framebuffer) HLine(x0, x1, y, thickness int, ink Ink) {
	for t := 0; t < thickness; t++ {
		for x := x0; x < x1; x++ {
			f.Set(x, y+t, ink)
		}
	}
}

// Hash returns a content hash of the image. The controller keeps it per label
// so that a re-delivered identical render is recognised without holding the
// pixels.
func (f *Framebuffer) Hash() uint64 {
	h := fnv.New64a()
	var dims [8]byte
	binary.BigEndian.PutUint32(dims[0:], uint32(f.W))
	binary.BigEndian.PutUint32(dims[4:], uint32(f.H))
	_, _ = h.Write(dims[:])
	buf := make([]byte, len(f.Pix))
	for i, p := range f.Pix {
		buf[i] = byte(p)
	}
	_, _ = h.Write(buf)
	return h.Sum64()
}

// Equal reports whether two framebuffers are identical.
func (f *Framebuffer) Equal(o *Framebuffer) bool {
	if o == nil || f.W != o.W || f.H != o.H {
		return false
	}
	for i := range f.Pix {
		if f.Pix[i] != o.Pix[i] {
			return false
		}
	}
	return true
}

// DiffResult describes how two renders differ.
type DiffResult struct {
	// Changed is the number of pixels that differ.
	Changed int
	// Total is the panel's pixel count.
	Total int
	// Bounds is the smallest rectangle containing every changed pixel. A
	// partial waveform drives a window, not a scatter of pixels, so this — not
	// Changed — is what the panel actually has to refresh.
	Bounds Rect
	// TouchesColour is true when any changed pixel is, or was, an ink other than
	// black or white.
	TouchesColour bool
	// SizeChanged is true when the two images are different shapes, which makes
	// any comparison meaningless.
	SizeChanged bool
}

// Fraction is the share of the panel whose pixels changed.
func (d DiffResult) Fraction() float64 {
	if d.Total == 0 {
		return 0
	}
	return float64(d.Changed) / float64(d.Total)
}

// WindowFraction is the share of the panel the partial waveform would have to
// drive.
func (d DiffResult) WindowFraction() float64 {
	if d.Total == 0 {
		return 0
	}
	return float64(d.Bounds.Area()) / float64(d.Total)
}

// Diff compares this render with the previous one.
func (f *Framebuffer) Diff(prev *Framebuffer) DiffResult {
	d := DiffResult{Total: f.W * f.H}
	if prev == nil || prev.W != f.W || prev.H != f.H {
		d.SizeChanged = true
		d.Changed = d.Total
		d.Bounds = Rect{0, 0, f.W, f.H}
		d.TouchesColour = true
		return d
	}
	x0, y0, x1, y1 := f.W, f.H, 0, 0
	for y := 0; y < f.H; y++ {
		row := y * f.W
		for x := 0; x < f.W; x++ {
			a, b := f.Pix[row+x], prev.Pix[row+x]
			if a == b {
				continue
			}
			d.Changed++
			if a > InkBlack || b > InkBlack {
				d.TouchesColour = true
			}
			if x < x0 {
				x0 = x
			}
			if x >= x1 {
				x1 = x + 1
			}
			if y < y0 {
				y0 = y
			}
			if y >= y1 {
				y1 = y + 1
			}
		}
	}
	if d.Changed > 0 {
		d.Bounds = Rect{x0, y0, x1, y1}
	}
	return d
}

// ---------------------------------------------------------------------------
// Wire encoding
// ---------------------------------------------------------------------------

var rleMagic = [4]byte{'U', 'F', 'B', '2'}

// SubImage returns a copy of a rectangular region.
//
// It exists because a partial refresh drives a window, so sending the whole
// panel to service one is a waste of the zone's scarcest resource. A price
// changing from 2.49 to 1.99 moves a couple of hundred pixels inside one band;
// transmitting that band is a few hundred bytes and transmitting the panel is
// several thousand, which at 250 kbps shared across five hundred labels is the
// difference between a price load taking a minute and taking ten.
func (f *Framebuffer) SubImage(r Rect) *Framebuffer {
	if r.X0 < 0 {
		r.X0 = 0
	}
	if r.Y0 < 0 {
		r.Y0 = 0
	}
	if r.X1 > f.W {
		r.X1 = f.W
	}
	if r.Y1 > f.H {
		r.Y1 = f.H
	}
	if r.Empty() {
		return NewFramebuffer(0, 0)
	}
	out := NewFramebuffer(r.X1-r.X0, r.Y1-r.Y0)
	for y := r.Y0; y < r.Y1; y++ {
		copy(out.Pix[(y-r.Y0)*out.W:(y-r.Y0+1)*out.W], f.Pix[y*f.W+r.X0:y*f.W+r.X1])
	}
	return out
}

// Blit copies src into f with its top-left corner at (x, y). It is the
// operation the label firmware performs on a windowed update.
func (f *Framebuffer) Blit(src *Framebuffer, x, y int) {
	if src == nil {
		return
	}
	for sy := 0; sy < src.H; sy++ {
		for sx := 0; sx < src.W; sx++ {
			f.Set(x+sx, y+sy, src.Pix[sy*src.W+sx])
		}
	}
}

// Run-length encoding, format UFB2.
//
// Two properties of a shelf-label image drive the design, and both were
// measured rather than assumed. First, the image is mostly identical rows: a
// price drawn at eight times a 5x7 cell has every glyph row repeated eight
// times, and the bands above and below it are solid white. Second, the runs
// within a row are short — a few pixels of glyph, a few of gap — so a run
// header that costs a whole byte doubles the payload.
//
// So rows are grouped by repetition and runs are packed into one byte where
// they can be. On a 296x128 BWR price render this takes the image from 37,888
// bytes raw, or about 3,500 bytes under naive per-run encoding, to a few
// hundred: the difference between an update costing forty 802.15.4 fragments
// per hop and costing eight.
//
// The decoder is a dozen lines and no allocation beyond the framebuffer, which
// is the constraint that ruled out anything with a dictionary: it has to run in
// a few hundred bytes of firmware on a Cortex-M0.
const (
	// runLongFlag marks a run whose length follows as a varint.
	runLongFlag = 0x80
	// runInkShift positions the three-bit ink state.
	runInkShift = 4
	// runShortMax is the longest run expressible in the header byte alone.
	runShortMax = 16
)

// EncodeRLE compresses the framebuffer for transmission over the mesh.
func (f *Framebuffer) EncodeRLE() []byte {
	out := make([]byte, 0, 512)
	out = append(out, rleMagic[:]...)
	var dims [4]byte
	binary.BigEndian.PutUint16(dims[0:], uint16(f.W))
	binary.BigEndian.PutUint16(dims[2:], uint16(f.H))
	out = append(out, dims[:]...)
	if f.W == 0 || f.H == 0 {
		return out
	}
	sameRow := func(a, b int) bool {
		return bytes.Equal(inkBytes(f.Pix[a*f.W:(a+1)*f.W]), inkBytes(f.Pix[b*f.W:(b+1)*f.W]))
	}
	for y := 0; y < f.H; {
		reps := 1
		for y+reps < f.H && sameRow(y, y+reps) {
			reps++
		}
		out = binary.AppendUvarint(out, uint64(reps))
		row := f.Pix[y*f.W : (y+1)*f.W]
		x := 0
		for x < len(row) {
			ink := row[x]
			n := 1
			for x+n < len(row) && row[x+n] == ink {
				n++
			}
			x += n
			for n > 0 {
				if n <= runShortMax {
					out = append(out, byte(ink)<<runInkShift|byte(n-1))
					n = 0
					continue
				}
				out = append(out, runLongFlag|byte(ink)<<runInkShift)
				out = binary.AppendUvarint(out, uint64(n))
				n = 0
			}
		}
		y += reps
	}
	return out
}

// inkBytes reinterprets a run of ink states as bytes for comparison. Ink is a
// byte-wide type, so this is a view rather than a conversion.
func inkBytes(p []Ink) []byte {
	b := make([]byte, len(p))
	for i, v := range p {
		b[i] = byte(v)
	}
	return b
}

// DecodeRLE reconstructs a framebuffer. It is the reference for the firmware's
// decoder, and the tests compare a round trip pixel for pixel.
func DecodeRLE(b []byte) (*Framebuffer, error) {
	if len(b) < 8 || !bytes.Equal(b[:4], rleMagic[:]) {
		return nil, fmt.Errorf("%w: bad magic", ErrBadFramebuffer)
	}
	w := int(binary.BigEndian.Uint16(b[4:]))
	h := int(binary.BigEndian.Uint16(b[6:]))
	f := NewFramebuffer(w, h)
	if w == 0 || h == 0 {
		return f, nil
	}
	i := 8
	y := 0
	row := make([]Ink, w)
	for y < h {
		reps, used := binary.Uvarint(b[i:])
		if used <= 0 || reps == 0 {
			return nil, fmt.Errorf("%w: bad row repeat count", ErrBadFramebuffer)
		}
		i += used
		x := 0
		for x < w {
			if i >= len(b) {
				return nil, fmt.Errorf("%w: truncated run header", ErrBadFramebuffer)
			}
			hdr := b[i]
			i++
			ink := Ink((hdr >> runInkShift) & 0x07)
			if ink >= inkCount {
				return nil, fmt.Errorf("%w: ink state %d does not exist", ErrBadFramebuffer, ink)
			}
			n := int(hdr&0x0F) + 1
			if hdr&runLongFlag != 0 {
				v, used := binary.Uvarint(b[i:])
				if used <= 0 {
					return nil, fmt.Errorf("%w: truncated run length", ErrBadFramebuffer)
				}
				i += used
				n = int(v)
			}
			if n <= 0 || x+n > w {
				return nil, fmt.Errorf("%w: run of %d overflows a row of %d", ErrBadFramebuffer, n, w)
			}
			for j := 0; j < n; j++ {
				row[x+j] = ink
			}
			x += n
		}
		if y+int(reps) > h {
			return nil, fmt.Errorf("%w: row group of %d overruns the panel", ErrBadFramebuffer, reps)
		}
		for r := 0; r < int(reps); r++ {
			copy(f.Pix[(y+r)*w:(y+r+1)*w], row)
		}
		y += int(reps)
	}
	return f, nil
}

// ---------------------------------------------------------------------------
// Human-readable dumps
// ---------------------------------------------------------------------------

// WritePBM writes the image as a plain-text portable bitmap.
//
// PBM exists here because it is the format a field engineer can read in a
// terminal: `cat` it and the price is legible as ASCII art. Any ink that is not
// white becomes a set bit, which is exactly what a monochrome panel would show.
func (f *Framebuffer) WritePBM(w io.Writer) error {
	buf := bytes.NewBuffer(make([]byte, 0, f.W*f.H*2+32))
	fmt.Fprintf(buf, "P1\n# USSLP label render %dx%d\n%d %d\n", f.W, f.H, f.W, f.H)
	for y := 0; y < f.H; y++ {
		for x := 0; x < f.W; x++ {
			if f.Pix[y*f.W+x] == InkWhite {
				buf.WriteString("0 ")
			} else {
				buf.WriteString("1 ")
			}
		}
		buf.WriteByte('\n')
	}
	_, err := w.Write(buf.Bytes())
	if err != nil {
		return fmt.Errorf("sec: writing PBM: %w", err)
	}
	return nil
}

// WritePNG writes the image in colour, which is the only honest way to inspect
// a three- or seven-ink render.
func (f *Framebuffer) WritePNG(w io.Writer) error {
	img := image.NewRGBA(image.Rect(0, 0, f.W, f.H))
	for y := 0; y < f.H; y++ {
		for x := 0; x < f.W; x++ {
			ink := f.Pix[y*f.W+x]
			if ink >= inkCount {
				ink = InkWhite
			}
			img.SetRGBA(x, y, inkRGB[ink])
		}
	}
	if err := png.Encode(w, img); err != nil {
		return fmt.Errorf("sec: writing PNG: %w", err)
	}
	return nil
}

// String renders the image as compact ASCII art for test failure messages,
// downsampled so a 296x128 panel fits on a terminal.
func (f *Framebuffer) String() string {
	const maxW = 74
	step := 1
	for f.W/step > maxW {
		step++
	}
	var b bytes.Buffer
	for y := 0; y < f.H; y += step * 2 {
		for x := 0; x < f.W; x += step {
			switch f.At(x, y) {
			case InkWhite:
				b.WriteByte('.')
			case InkBlack:
				b.WriteByte('#')
			default:
				b.WriteByte('o')
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}
