package gitproto

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/go-vcs/gitproto/pktline"
)

// Limits bound what one request may cost. The zero value of a field means the
// default; a negative value is refused by Validate.
type Limits struct {
	// MaxRequestBytes bounds the bytes read for one command request.
	MaxRequestBytes int64
	// MaxRequestLines bounds the pkt-lines of one command request.
	MaxRequestLines int
	// MaxWants bounds "want" lines of a fetch request.
	MaxWants int
	// MaxHaves bounds "have" lines of a fetch request.
	MaxHaves int
	// MaxRefPrefixes bounds "ref-prefix" lines of an ls-refs request.
	MaxRefPrefixes int
}

// DefaultLimits are used for every zero field of Limits.
var DefaultLimits = Limits{
	MaxRequestBytes: 8 << 20,
	MaxRequestLines: 100_000,
	MaxWants:        50_000,
	MaxHaves:        50_000,
	MaxRefPrefixes:  1024,
}

func (l Limits) withDefaults() Limits {
	if l.MaxRequestBytes == 0 {
		l.MaxRequestBytes = DefaultLimits.MaxRequestBytes
	}
	if l.MaxRequestLines == 0 {
		l.MaxRequestLines = DefaultLimits.MaxRequestLines
	}
	if l.MaxWants == 0 {
		l.MaxWants = DefaultLimits.MaxWants
	}
	if l.MaxHaves == 0 {
		l.MaxHaves = DefaultLimits.MaxHaves
	}
	if l.MaxRefPrefixes == 0 {
		l.MaxRefPrefixes = DefaultLimits.MaxRefPrefixes
	}
	return l
}

// Validate refuses negative limits.
func (l Limits) Validate() error {
	if l.MaxRequestBytes < 0 || l.MaxRequestLines < 0 || l.MaxWants < 0 || l.MaxHaves < 0 || l.MaxRefPrefixes < 0 {
		return errors.New("gitproto: negative limit")
	}
	return nil
}

// ProtocolError is a refusal caused by the client's request. It is reported
// to the client as an "ERR <message>" pkt-line.
type ProtocolError struct{ Msg string }

func (e *ProtocolError) Error() string { return "gitproto: " + e.Msg }

func protoErr(format string, a ...any) error {
	return &ProtocolError{Msg: fmt.Sprintf(format, a...)}
}

// Request is a parsed command request (gitprotocol-v2: command-request).
type Request struct {
	// Command is the value of "command=".
	Command string
	// Agent is the client's agent string, if it sent one.
	Agent string
	// Args are the command-specific argument lines, LF stripped, after the
	// delim-pkt.
	Args []string
}

// advertisedCapability reports whether a client may send key=value in a
// request, given what Advertise sends.
func checkCapability(key, value string) error {
	switch key {
	case "agent":
		if value == "" {
			return protoErr("empty agent")
		}
		for i := 0; i < len(value); i++ {
			if value[i] <= 32 || value[i] >= 127 {
				return protoErr("agent contains a non-printable or space byte")
			}
		}
		return nil
	case "object-format":
		if value != "sha1" {
			return protoErr("unsupported object-format %q (only sha1)", clip(value))
		}
		return nil
	}
	return protoErr("unsupported capability %q", clip(key))
}

func clip(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}

// ParseRequest reads one command request from r. It returns io.EOF for a
// clean end of session: end of input between requests, or the empty request
// (a lone flush-pkt). Every other malformed input is a *ProtocolError, except
// for transport failures, which are returned as read.
func ParseRequest(r *pktline.Reader, lim Limits) (*Request, error) {
	lim = lim.withDefaults()
	start := r.Consumed()
	lines := 0
	next := func() (pktline.Packet, error) {
		p, err := r.Read()
		if err != nil {
			return p, err
		}
		lines++
		if lines > lim.MaxRequestLines {
			return p, protoErr("request exceeds %d lines", lim.MaxRequestLines)
		}
		if r.Consumed()-start > lim.MaxRequestBytes {
			return p, protoErr("request exceeds %d bytes", lim.MaxRequestBytes)
		}
		if p.Kind == pktline.Data && bytes.IndexByte(p.Data, 0) >= 0 {
			return p, protoErr("NUL byte in request line")
		}
		return p, nil
	}
	fail := func(err error) (*Request, error) {
		if errors.Is(err, pktline.ErrBadLength) || errors.Is(err, pktline.ErrReserved) || errors.Is(err, pktline.ErrTooLong) {
			return nil, protoErr("malformed pkt-line: %v", err)
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, protoErr("request truncated")
		}
		return nil, err
	}

	p, err := next()
	if err != nil {
		return fail(err)
	}
	switch p.Kind {
	case pktline.Flush:
		return nil, io.EOF
	case pktline.Data:
	default:
		return nil, protoErr("unexpected %s packet at start of request", p.Kind)
	}
	line := string(pktline.Text(p.Data))
	cmd, ok := strings.CutPrefix(line, "command=")
	if !ok {
		return nil, protoErr("request does not start with command=")
	}
	if !validKey(cmd) {
		return nil, protoErr("invalid command name %q", clip(cmd))
	}
	req := &Request{Command: cmd}

	// capability-list, up to delim-pkt (arguments follow) or flush-pkt.
	inArgs := false
	sawObjectFormat := false
	for {
		p, err := next()
		if err != nil {
			return fail(err)
		}
		if p.Kind == pktline.Flush {
			return req, nil
		}
		if p.Kind == pktline.Delim {
			if inArgs {
				return nil, protoErr("second delim-pkt in request")
			}
			inArgs = true
			continue
		}
		if p.Kind == pktline.ResponseEnd {
			return nil, protoErr("unexpected response-end packet in request")
		}
		line := string(pktline.Text(p.Data))
		if inArgs {
			req.Args = append(req.Args, line)
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		if !validKey(key) {
			return nil, protoErr("invalid capability %q", clip(key))
		}
		if err := checkCapability(key, value); err != nil {
			return nil, err
		}
		switch key {
		case "agent":
			if req.Agent != "" {
				return nil, protoErr("duplicate agent capability")
			}
			req.Agent = value
		case "object-format":
			if sawObjectFormat {
				return nil, protoErr("duplicate object-format capability")
			}
			sawObjectFormat = true
		}
	}
}

func validKey(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}
