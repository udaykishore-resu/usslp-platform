// Package codepage transcodes the legacy character encodings that enterprise
// retail data still arrives in.
//
// It exists because Go's encoding/xml refuses outright any document whose
// declaration names an encoding it does not know, and the two largest ERP
// sources the platform ingests — SAP ALE ports and Oracle RIB — both emit
// ISO-8859-1 or Windows-1252 whenever the system's code page is a European one
// (SAP 1100/1160, and an Oracle database with a WE8 character set). Without a
// reader here, every price message from a correctly configured German or French
// installation fails to parse, with an error that reads like a Go problem
// rather than a code-page one. The nightly flat files an AS/400 writes have the
// same property and no declaration at all, which is why Decode exists beside
// Reader: for a file, the encoding is configuration rather than something the
// data announces.
//
// The consequence of getting it wrong is not only a parse failure. A product
// description containing an umlaut decoded as raw bytes becomes mojibake on a
// shelf label, and a label showing corrupted text is one a store manager
// unplugs.
package codepage

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Reader is an xml.Decoder.CharsetReader: it transcodes a document's declared
// code page into the UTF-8 that encoding/xml requires.
//
// An unknown code page is an error rather than a pass-through. Passing the
// bytes through unchanged would produce a document that parses and is subtly
// wrong, and "subtly wrong" on the price path is the outcome this package
// exists to prevent.
func Reader(charset string, input io.Reader) (io.Reader, error) {
	switch normaliseCharset(charset) {
	case "utf-8", "us-ascii", "ascii", "":
		return input, nil
	case "iso-8859-1", "latin1", "iso-8859-15", "latin9", "cp819":
		return newSingleByteReader(input, latin1Table), nil
	case "windows-1252", "cp1252":
		return newSingleByteReader(input, cp1252Table), nil
	default:
		return nil, fmt.Errorf("unsupported XML code page %q", charset)
	}
}

func normaliseCharset(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	c = strings.ReplaceAll(c, "_", "-")
	return c
}

// singleByteTable maps each of the 256 byte values to a rune. nil entries in
// the high half fall back to the byte's own value, which is exactly the
// ISO-8859-1 rule.
type singleByteTable [256]rune

var latin1Table = func() singleByteTable {
	var t singleByteTable
	for i := 0; i < 256; i++ {
		// ISO-8859-1 is the identity mapping onto the first 256 code points,
		// and ISO-8859-15 differs in eight positions that do not appear in SAP
		// numeric or material fields; treating them alike is safe for the
		// fields the adapter reads and avoids failing a whole IDoc over a
		// currency symbol in a description.
		t[i] = rune(i)
	}
	return t
}()

var cp1252Table = func() singleByteTable {
	t := latin1Table
	// Windows-1252 differs from ISO-8859-1 only in 0x80–0x9F, where Latin-1 has
	// unused control codes and Windows has punctuation. Systems on Windows
	// presentation servers emit these in descriptions — a right single quote in
	// a product name is 0x92 — and mapping them to control characters would put
	// an unprintable byte on a label.
	for b, r := range map[int]rune{
		0x80: '€', 0x82: '‚', 0x83: 'ƒ', 0x84: '„',
		0x85: '…', 0x86: '†', 0x87: '‡', 0x88: 'ˆ',
		0x89: '‰', 0x8A: 'Š', 0x8B: '‹', 0x8C: 'Œ',
		0x8E: 'Ž', 0x91: '‘', 0x92: '’', 0x93: '“',
		0x94: '”', 0x95: '•', 0x96: '–', 0x97: '—',
		0x98: '˜', 0x99: '™', 0x9A: 'š', 0x9B: '›',
		0x9C: 'œ', 0x9E: 'ž', 0x9F: 'Ÿ',
	} {
		t[b] = r
	}
	return t
}()

// newSingleByteReader transcodes eagerly into a buffer.
//
// Eagerly rather than streaming because these are bounded messages — the
// gateway caps request bodies long before this — and a streaming transcoder has
// to deal with a multi-byte rune straddling a read boundary, which is an
// entirely avoidable class of bug on a path that must never corrupt a price.
func newSingleByteReader(r io.Reader, table singleByteTable) io.Reader {
	raw, err := io.ReadAll(r)
	if err != nil {
		return errReader{err}
	}
	var buf bytes.Buffer
	buf.Grow(len(raw) + len(raw)/4)
	var enc [utf8.UTFMax]byte
	for _, b := range raw {
		r := table[b]
		if r < utf8.RuneSelf {
			buf.WriteByte(byte(r))
			continue
		}
		n := utf8.EncodeRune(enc[:], r)
		buf.Write(enc[:n])
	}
	return &buf
}

// errReader defers a read failure to the XML decoder, which reports it with the
// document position rather than losing it inside the charset reader.
type errReader struct{ err error }

// Read always fails with the deferred error.
func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// Decode transcodes a whole payload whose encoding is known from configuration
// rather than declared in the data.
//
// Flat-file drops carry no encoding declaration, so the binding states it. An
// unknown name is an error for the same reason it is in Reader: a file
// transcoded wrongly still parses, and a SKU or a description that is subtly
// wrong is worse than one that fails loudly.
func Decode(name string, b []byte) ([]byte, error) {
	r, err := Reader(name, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}
