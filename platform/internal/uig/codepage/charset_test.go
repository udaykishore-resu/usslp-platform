package codepage

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"
)

func TestDecodeLatin1AndCP1252(t *testing.T) {
	// 0xFC is ü in ISO-8859-1 and in Windows-1252; 0x92 is an unused control
	// code in Latin-1 and a right single quote in Windows-1252. Getting the
	// second one wrong puts an unprintable byte on a shelf label.
	latin1 := []byte("M\xfcsli")
	got, err := Decode("iso-8859-1", latin1)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if string(got) != "Müsli" {
		t.Errorf("iso-8859-1 = %q, want %q", got, "Müsli")
	}

	win := []byte("Bob\x92s Beans \x80")
	got, err = Decode("windows-1252", win)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if string(got) != "Bob’s Beans €" {
		t.Errorf("windows-1252 = %q", got)
	}
	// The same bytes read as Latin-1 give control characters, which is exactly
	// why the encoding is configuration rather than a guess.
	got, err = Decode("iso-8859-1", win)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "’") {
		t.Error("latin-1 decoding invented a curly quote")
	}
}

func TestDecodePassThroughAndUnknown(t *testing.T) {
	for _, name := range []string{"utf-8", "UTF-8", "us-ascii", "", "ASCII"} {
		got, err := Decode(name, []byte("plain"))
		if err != nil || string(got) != "plain" {
			t.Errorf("Decode(%q) = %q, %v", name, got, err)
		}
	}
	// An unknown code page is an error, not a pass-through: bytes handed
	// through unchanged produce a document that parses and is subtly wrong.
	if _, err := Decode("ebcdic-cp-be", []byte("x")); err == nil {
		t.Error("an unknown code page was accepted")
	}
	if _, err := Decode("utf-32", []byte("x")); err == nil {
		t.Error("an unsupported code page was accepted")
	}
}

func TestReaderLetsEncodingXMLParseALegacyDocument(t *testing.T) {
	doc := []byte(`<?xml version="1.0" encoding="ISO-8859-1"?><item><name>Caf` + "\xe9" + `</name></item>`)

	// Without a CharsetReader, Go refuses the document outright — which is the
	// failure mode every SAP and Oracle integration hits on day one.
	var probe struct {
		Name string `xml:"name"`
	}
	plain := xml.NewDecoder(bytes.NewReader(doc))
	if err := plain.Decode(&probe); err == nil {
		t.Fatal("encoding/xml accepted a declared ISO-8859-1 document with no CharsetReader")
	}

	dec := xml.NewDecoder(bytes.NewReader(doc))
	dec.CharsetReader = Reader
	if err := dec.Decode(&probe); err != nil {
		t.Fatalf("Decode with CharsetReader: %v", err)
	}
	if probe.Name != "Café" {
		t.Errorf("name = %q, want Café", probe.Name)
	}
}

func TestReaderRefusesAnUnknownDeclaration(t *testing.T) {
	doc := []byte(`<?xml version="1.0" encoding="EBCDIC-CP-BE"?><item/>`)
	dec := xml.NewDecoder(bytes.NewReader(doc))
	dec.CharsetReader = Reader
	var probe struct{}
	if err := dec.Decode(&probe); err == nil {
		t.Fatal("an unknown code page was silently accepted")
	}
}

func TestNormaliseCharsetAcceptsCommonSpellings(t *testing.T) {
	for _, name := range []string{"ISO-8859-1", "iso_8859_1", " latin1 ", "cp819", "ISO-8859-15"} {
		if _, err := Decode(name, []byte("\xfc")); err != nil {
			t.Errorf("Decode(%q): %v", name, err)
		}
	}
}
