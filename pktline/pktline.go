// Package pktline implements Git's pkt-line framing as specified by
// gitprotocol-common(5) and the protocol v2 special packets of
// gitprotocol-v2(5): flush-pkt "0000", delim-pkt "0001" and
// response-end-pkt "0002".
//
// The reader is bounded: a packet longer than MaxPacket (65520 bytes, header
// included) is refused before any payload is read or allocated, and so is any
// length field that is not four lowercase hexadecimal digits or that falls on
// the unassigned values 0003.
package pktline

import (
	"errors"
	"fmt"
	"io"
)

const (
	// MaxPacket is the largest pkt-line, length header included.
	MaxPacket = 65520
	// MaxData is the largest payload of a data pkt-line.
	MaxData = MaxPacket - 4
	// MaxSidebandData is the largest payload that fits in a pkt-line that
	// also carries a one-byte side-band stream code.
	MaxSidebandData = MaxData - 1
)

// Kind is the kind of a packet.
type Kind int

const (
	// Data is a data pkt-line (possibly empty: "0004").
	Data Kind = iota
	// Flush is "0000".
	Flush
	// Delim is "0001".
	Delim
	// ResponseEnd is "0002".
	ResponseEnd
)

func (k Kind) String() string {
	switch k {
	case Data:
		return "data"
	case Flush:
		return "flush"
	case Delim:
		return "delim"
	case ResponseEnd:
		return "response-end"
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// Refusals. All are wrapped with %w by the reader and writer.
var (
	ErrBadLength  = errors.New("pktline: length field is not four lowercase hex digits")
	ErrReserved   = errors.New("pktline: reserved length 0003")
	ErrTooLong    = errors.New("pktline: packet exceeds 65520 bytes")
	ErrDataTooBig = errors.New("pktline: payload exceeds 65516 bytes")
)

// Packet is one decoded packet. Data aliases the reader's buffer and is valid
// until the next call to Read.
type Packet struct {
	Kind Kind
	Data []byte
}

// Reader decodes pkt-lines from an io.Reader.
type Reader struct {
	r   io.Reader
	hdr [4]byte
	buf [MaxData]byte
	n   int64 // bytes consumed from r, headers included
}

// NewReader returns a Reader. r should be buffered by the caller if it is
// expensive to read from in small pieces.
func NewReader(r io.Reader) *Reader { return &Reader{r: r} }

// Consumed returns the number of bytes read from the underlying reader.
func (r *Reader) Consumed() int64 { return r.n }

// Read returns the next packet. It returns io.EOF only when the stream ends
// exactly between two packets; a stream cut inside a packet yields
// io.ErrUnexpectedEOF.
func (r *Reader) Read() (Packet, error) {
	if _, err := io.ReadFull(r.r, r.hdr[:]); err != nil {
		if err == io.EOF {
			return Packet{}, io.EOF
		}
		return Packet{}, fmt.Errorf("pktline: reading length: %w", err)
	}
	r.n += 4
	n, err := parseLen(r.hdr)
	if err != nil {
		return Packet{}, err
	}
	switch {
	case n == 0:
		return Packet{Kind: Flush}, nil
	case n == 1:
		return Packet{Kind: Delim}, nil
	case n == 2:
		return Packet{Kind: ResponseEnd}, nil
	}
	// parseLen guarantees 4 <= n <= MaxPacket; clamping keeps the slice in
	// range whatever happens to that guarantee.
	size := min(max(n-4, 0), len(r.buf))
	if _, err := io.ReadFull(r.r, r.buf[:size]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return Packet{}, fmt.Errorf("pktline: reading payload: %w", err)
	}
	r.n += int64(size)
	return Packet{Kind: Data, Data: r.buf[:size]}, nil
}

func parseLen(h [4]byte) (int, error) {
	n := 0
	for _, c := range h {
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		default:
			return 0, fmt.Errorf("%w: %q", ErrBadLength, h[:])
		}
		n = n<<4 | d
	}
	if n == 3 {
		return 0, ErrReserved
	}
	if n > MaxPacket {
		return 0, fmt.Errorf("%w: %d", ErrTooLong, n)
	}
	return n, nil
}

// Text returns the payload of a data packet with one trailing LF removed, as
// gitprotocol-common(5) requires receivers to do.
func Text(p []byte) []byte {
	if n := len(p); n > 0 && p[n-1] == '\n' {
		return p[:n-1]
	}
	return p
}

// Writer encodes pkt-lines.
type Writer struct {
	w   io.Writer
	err error  // sticky: the first write error
	buf []byte // reused packet frame
}

// Err returns the first write error, if any. Once set, every later write
// returns it without touching the underlying writer.
func (w *Writer) Err() error { return w.err }

func (w *Writer) put(b []byte) error {
	if w.err != nil {
		return w.err
	}
	_, w.err = w.w.Write(b)
	return w.err
}

// NewWriter returns a Writer.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

const hexdigits = "0123456789abcdef"

// Data writes one data pkt-line. A payload over MaxData is refused.
func (w *Writer) Data(p []byte) error {
	if len(p) > MaxData {
		return fmt.Errorf("%w: %d", ErrDataTooBig, len(p))
	}
	// One Write call per packet so that unbuffered transports see whole packets.
	n := len(p) + 4
	b := append(w.buf[:0], hexdigits[n>>12&15], hexdigits[n>>8&15], hexdigits[n>>4&15], hexdigits[n&15])
	b = append(b, p...)
	w.buf = b
	return w.put(b)
}

// Text writes s followed by LF as one data pkt-line.
func (w *Writer) Text(s string) error {
	return w.Data([]byte(s + "\n"))
}

// Sideband writes p on side-band channel ch (1, 2 or 3), splitting it into
// packets of at most MaxSidebandData payload bytes.
func (w *Writer) Sideband(ch byte, p []byte) error {
	if ch < 1 || ch > 3 {
		return fmt.Errorf("pktline: invalid side-band channel %d", ch)
	}
	for len(p) > 0 {
		n := min(len(p), MaxSidebandData)
		// One reused frame buffer: io.Writer forbids retaining p, so the
		// next packet may overwrite it.
		b := append(w.buf[:0], hexdigits[(n+5)>>12&15], hexdigits[(n+5)>>8&15], hexdigits[(n+5)>>4&15], hexdigits[(n+5)&15], ch)
		b = append(b, p[:n]...)
		w.buf = b
		if err := w.put(b); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// Flush writes "0000".
func (w *Writer) Flush() error { return w.special("0000") }

// Delim writes "0001".
func (w *Writer) Delim() error { return w.special("0001") }

// ResponseEnd writes "0002".
func (w *Writer) ResponseEnd() error { return w.special("0002") }

func (w *Writer) special(s string) error {
	return w.put([]byte(s))
}
