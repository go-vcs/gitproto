package pktline

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadSpecExamples(t *testing.T) {
	// gitprotocol-common(5) examples.
	for in, want := range map[string]string{
		"0006a\n":      "a\n",
		"0005a":        "a",
		"000bfoobar\n": "foobar\n",
		"0004":         "",
	} {
		p, err := NewReader(strings.NewReader(in)).Read()
		if err != nil || p.Kind != Data || string(p.Data) != want {
			t.Errorf("%q: got %v %q %v", in, p.Kind, p.Data, err)
		}
	}
}

func TestReadSpecials(t *testing.T) {
	r := NewReader(strings.NewReader("000000010002"))
	for _, k := range []Kind{Flush, Delim, ResponseEnd} {
		p, err := r.Read()
		if err != nil || p.Kind != k {
			t.Fatalf("want %v got %v %v", k, p.Kind, err)
		}
	}
	if _, err := r.Read(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestReadRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want error
	}{
		"reserved 0003":      {"0003", ErrReserved},
		"over max ffff":      {"ffff", ErrTooLong},
		"over max by one":    {"fff1", ErrTooLong},
		"uppercase hex":      {"000A" + strings.Repeat("x", 6), ErrBadLength},
		"non hex":            {"00zz", ErrBadLength},
		"sign":               {"+005a", ErrBadLength},
		"space":              {" 005a", ErrBadLength},
		"truncated header":   {"00", io.ErrUnexpectedEOF},
		"truncated payload":  {"000aabc", io.ErrUnexpectedEOF},
		"header only, no pl": {"0010", io.ErrUnexpectedEOF},
	} {
		_, err := NewReader(strings.NewReader(tc.in)).Read()
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}

func TestMaxPacketAccepted(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, MaxData)
	var buf bytes.Buffer
	if err := NewWriter(&buf).Data(payload); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != MaxPacket || !strings.HasPrefix(buf.String(), "fff0") {
		t.Fatalf("len %d prefix %q", buf.Len(), buf.String()[:4])
	}
	p, err := NewReader(&buf).Read()
	if err != nil || len(p.Data) != MaxData {
		t.Fatalf("%v %d", err, len(p.Data))
	}
}

func TestWriterRefusals(t *testing.T) {
	w := NewWriter(io.Discard)
	if err := w.Data(make([]byte, MaxData+1)); !errors.Is(err, ErrDataTooBig) {
		t.Errorf("Data: %v", err)
	}
	if err := w.Text(strings.Repeat("x", MaxData)); !errors.Is(err, ErrDataTooBig) {
		t.Errorf("Text: %v", err)
	}
	if err := w.Sideband(0, []byte("x")); err == nil {
		t.Error("channel 0 accepted")
	}
	if err := w.Sideband(4, []byte("x")); err == nil {
		t.Error("channel 4 accepted")
	}
}

type countingFail struct{ calls int }

func (c *countingFail) Write([]byte) (int, error) { c.calls++; return 0, io.ErrClosedPipe }

// After the first failed write the Writer is dead: later writes return the
// same error and never reach the underlying writer.
func TestWriterErrorIsSticky(t *testing.T) {
	c := &countingFail{}
	w := NewWriter(c)
	if err := w.Text("a"); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("first write: %v", err)
	}
	if err := w.Flush(); !errors.Is(err, io.ErrClosedPipe) || c.calls != 1 {
		t.Fatalf("second write: %v after %d calls", err, c.calls)
	}
	if !errors.Is(w.Err(), io.ErrClosedPipe) {
		t.Fatalf("Err() = %v", w.Err())
	}
	if NewWriter(io.Discard).Err() != nil {
		t.Fatal("fresh Writer has an error")
	}
}

func TestTextBoundary(t *testing.T) {
	if err := NewWriter(io.Discard).Text(strings.Repeat("x", MaxData-1)); err != nil {
		t.Fatalf("largest text line refused: %v", err)
	}
}

func TestSidebandSplits(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	data := bytes.Repeat([]byte{'a'}, MaxSidebandData*2+7)
	if err := w.Sideband(1, data); err != nil {
		t.Fatal(err)
	}
	r := NewReader(&buf)
	var got []byte
	n := 0
	for {
		p, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil || p.Data[0] != 1 {
			t.Fatalf("%v %v", err, p)
		}
		got = append(got, p.Data[1:]...)
		n++
	}
	if n != 3 || !bytes.Equal(got, data) {
		t.Fatalf("packets %d, equal %v", n, bytes.Equal(got, data))
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	_ = w.Text("version 2")
	_ = w.Delim()
	_ = w.Data(nil)
	_ = w.ResponseEnd()
	_ = w.Flush()
	if got, want := buf.String(), "000eversion 2\n00010004"+"00020000"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestText(t *testing.T) {
	if string(Text([]byte("a\n"))) != "a" || string(Text([]byte("a"))) != "a" || string(Text([]byte("a\n\n"))) != "a\n" {
		t.Fatal("Text strips exactly one LF")
	}
}

// endless yields the same byte forever; a reader that is not bounded would
// never stop consuming it.
type endless struct{ n int64 }

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'f'
	}
	e.n += int64(len(p))
	return len(p), nil
}

// TestBombOverlongRefusedBeforePayload: an "ffff..." stream is refused after
// the four header bytes, without reading or allocating a payload.
func TestBombOverlongRefusedBeforePayload(t *testing.T) {
	src := &endless{}
	r := NewReader(src)
	_, err := r.Read()
	if !errors.Is(err, ErrTooLong) {
		t.Fatalf("got %v", err)
	}
	if r.Consumed() != 4 {
		t.Fatalf("consumed %d bytes", r.Consumed())
	}
}

func FuzzReader(f *testing.F) {
	for _, s := range []string{"0000", "0001", "0002", "0004", "0006a\n", "000bfoobar\n", "fff0", "ffff", "0003", "00zz", "", "0010ab"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		r := NewReader(bytes.NewReader(in))
		var total int64
		for i := 0; ; i++ {
			if i > len(in) {
				t.Fatal("the reader never reached the end of its input")
			}
			p, err := r.Read()
			if err != nil {
				break
			}
			if len(p.Data) > MaxData {
				t.Fatalf("payload %d exceeds bound", len(p.Data))
			}
			if p.Kind != Data && len(p.Data) != 0 {
				t.Fatalf("special packet with payload")
			}
			total += 4 + int64(len(p.Data))
		}
		if total > int64(len(in)) || r.Consumed() > int64(len(in)) {
			t.Fatalf("consumed more than the input")
		}
		// Whatever was accepted must re-encode identically.
		r = NewReader(bytes.NewReader(in))
		var out bytes.Buffer
		w := NewWriter(&out)
		for i := 0; ; i++ {
			if i > len(in) {
				t.Fatal("the reader never reached the end of its input")
			}
			p, err := r.Read()
			if err != nil {
				break
			}
			switch p.Kind {
			case Flush:
				_ = w.Flush()
			case Delim:
				_ = w.Delim()
			case ResponseEnd:
				_ = w.ResponseEnd()
			default:
				if err := w.Data(p.Data); err != nil {
					t.Fatal(err)
				}
			}
		}
		if !bytes.HasPrefix(in, out.Bytes()) {
			t.Fatalf("re-encoding differs from input prefix")
		}
	})
}
