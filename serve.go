// Package gitproto serves Git wire protocol v2 (fetch side) from a
// Repository, over any transport that gives an io.Reader and an io.Writer.
//
// It knows nothing about forges, users or permissions: the caller decides who
// may reach which repository and mounts Serve (stateful: ssh, stdio,
// git://) or Advertise plus ServeRequest (stateless: smart HTTP) on its
// transport.
//
// Supported: capability advertisement, ls-refs (peel, symrefs, ref-prefix,
// unborn), fetch (want, have, done, thin-pack, no-progress, include-tag,
// ofs-delta). Everything else is refused by name with an "ERR" pkt-line:
// shallow and deepen*, filter, want-ref, sideband-all, packfile-uris,
// wait-for-done, object-info, bundle-uri, server-option, any object format
// but sha1, and protocol versions 0 and 1.
package gitproto

import (
	"bufio"
	"context"
	"errors"
	"io"

	"github.com/go-vcs/gitproto/pktline"
)

// Server serves protocol v2 requests from Repo.
type Server struct {
	Repo   Repository
	Limits Limits
	// Agent is advertised as agent=<Agent>. Empty means "gitproto".
	Agent string
}

func (s *Server) agent() string {
	if s.Agent == "" {
		return "gitproto"
	}
	return s.Agent
}

// Advertise writes the capability advertisement: "version 2", the
// capabilities and a flush-pkt (gitprotocol-v2, "Capability Advertisement").
func (s *Server) Advertise(w io.Writer) error {
	pw := pktline.NewWriter(w)
	for _, l := range []string{
		"version 2",
		"agent=" + s.agent(),
		"ls-refs=unborn",
		"fetch",
		"object-format=sha1",
	} {
		if err := pw.Text(l); err != nil {
			return err
		}
	}
	return pw.Flush()
}

// Serve is a stateful session: it advertises, then answers command requests
// until the client sends the empty request or closes the connection. It
// returns nil on a clean end, a *ProtocolError after refusing a request (the
// client has been sent the "ERR" line), or the transport / repository error.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	if err := s.Limits.Validate(); err != nil {
		return err
	}
	bw := bufio.NewWriterSize(w, 64<<10)
	if err := s.Advertise(bw); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	pr := pktline.NewReader(bufio.NewReaderSize(r, 64<<10))
	for {
		err := s.serveOne(ctx, pr, bw)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// ServeRequest answers one command request read from r (the stateless-rpc
// case: no advertisement, no loop). It returns io.EOF when r holds the empty
// request or nothing.
func (s *Server) ServeRequest(ctx context.Context, r io.Reader, w io.Writer) error {
	if err := s.Limits.Validate(); err != nil {
		return err
	}
	bw := bufio.NewWriterSize(w, 64<<10)
	return s.serveOne(ctx, pktline.NewReader(bufio.NewReaderSize(r, 64<<10)), bw)
}

func (s *Server) serveOne(ctx context.Context, pr *pktline.Reader, bw *bufio.Writer) error {
	pw := pktline.NewWriter(bw)
	req, err := ParseRequest(pr, s.Limits)
	if err == nil {
		err = s.dispatch(ctx, pw, req)
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			return err
		}
		var pe *ProtocolError
		if errors.As(err, &pe) {
			_ = pw.Text("ERR " + pe.Msg)
		} else if !isTransport(err) {
			_ = pw.Text("ERR internal error")
		}
		_ = bw.Flush()
		return err
	}
	return bw.Flush()
}

func (s *Server) dispatch(ctx context.Context, pw *pktline.Writer, req *Request) error {
	switch req.Command {
	case "ls-refs":
		return serveLsRefs(ctx, pw, s.Repo, req.Args, s.Limits)
	case "fetch":
		return serveFetch(ctx, pw, s.Repo, req.Args, s.Limits)
	}
	return protoErr("unsupported command %q", clip(req.Command))
}

// isTransport reports whether err came from the connection rather than from
// the request or the repository, in which case writing to it is pointless.
func isTransport(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.ErrUnexpectedEOF)
}
