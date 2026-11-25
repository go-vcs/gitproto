package gitproto_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/go-vcs/gitproto"
	"github.com/go-vcs/gitproto/pktline"
)

func seedRequests() [][]byte {
	return [][]byte{
		request("ls-refs", []string{"agent=git/2.47", "object-format=sha1"}, "peel", "symrefs", "unborn", "ref-prefix refs/heads/"),
		fetchReq("thin-pack", "ofs-delta", "want "+idA, "have "+idB, "done"),
		fetchReq("shallow "+idA, "deepen 1", "filter blob:none"),
		[]byte("0000"), []byte("0001"), []byte("0002"), []byte("ffff"), []byte("0003"),
		[]byte("0014command=ls-refs\n0000"), nil,
	}
}

func FuzzParseRequest(f *testing.F) {
	for _, s := range seedRequests() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		lim := gitproto.Limits{MaxRequestBytes: 1 << 16, MaxRequestLines: 1000}
		r := pktline.NewReader(bytes.NewReader(in))
		for i := 0; i < 8; i++ {
			req, err := gitproto.ParseRequest(r, lim)
			if err != nil {
				var pe *gitproto.ProtocolError
				if !errors.As(err, &pe) && err != io.EOF {
					t.Fatalf("unclassified error %v", err)
				}
				return
			}
			if req.Command == "" || strings.ContainsAny(req.Command, " \n\x00") {
				t.Fatalf("bad command %q", req.Command)
			}
			if len(req.Args) > lim.MaxRequestLines {
				t.Fatalf("%d args", len(req.Args))
			}
		}
	})
}

func FuzzParseFetch(f *testing.F) {
	f.Add("want " + idA + "\nhave " + idB + "\ndone\nthin-pack")
	f.Add("shallow x\ndeepen 1")
	f.Add("want \nhave\ndone x")
	f.Fuzz(func(t *testing.T, in string) {
		lim := gitproto.Limits{MaxWants: 8, MaxHaves: 8}
		a, err := gitproto.ParseFetch(strings.Split(in, "\n"), lim)
		if err != nil {
			var pe *gitproto.ProtocolError
			if !errors.As(err, &pe) {
				t.Fatalf("unclassified error %v", err)
			}
			return
		}
		if len(a.Wants) > 8 || len(a.Haves) > 8 {
			t.Fatal("limit exceeded")
		}
		for _, id := range append(append([]string{}, a.Wants...), a.Haves...) {
			if !gitproto.ValidOID(id) {
				t.Fatalf("invalid id %q accepted", id)
			}
		}
	})
}

func FuzzParseLsRefs(f *testing.F) {
	f.Add("peel\nsymrefs\nunborn\nref-prefix refs/")
	f.Add("ref-prefix \nfrob")
	f.Fuzz(func(t *testing.T, in string) {
		a, err := gitproto.ParseLsRefs(strings.Split(in, "\n"), gitproto.Limits{MaxRefPrefixes: 4})
		if err != nil {
			var pe *gitproto.ProtocolError
			if !errors.As(err, &pe) {
				t.Fatalf("unclassified error %v", err)
			}
			return
		}
		if len(a.Prefixes) > 4 {
			t.Fatal("limit exceeded")
		}
	})
}

// FuzzServe drives the whole server over a fake repository: whatever the
// bytes, it must not panic, must only ever write well-formed pkt-lines, and
// must refuse with an ERR line rather than serve a non-tip want.
func FuzzServe(f *testing.F) {
	for _, s := range seedRequests() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		repo := &fakeRepo{
			refs:   []gitproto.Ref{{Name: "HEAD", ID: idA, Target: "refs/heads/main"}, {Name: "refs/heads/main", ID: idA}},
			common: []string{idB}, pack: []byte("PACK"),
		}
		s := &gitproto.Server{Repo: repo, Limits: gitproto.Limits{MaxRequestBytes: 1 << 16, MaxRequestLines: 1000, MaxWants: 16, MaxHaves: 16}}
		var out bytes.Buffer
		_ = s.Serve(context.Background(), bytes.NewReader(in), &out)
		outLen := out.Len()
		r := pktline.NewReader(&out)
		for i := 0; ; i++ {
			if i > outLen {
				t.Fatal("the reader never reached the end of the output")
			}
			if _, err := r.Read(); err != nil {
				if err != io.EOF {
					t.Fatalf("server wrote a malformed pkt-line: %v", err)
				}
				break
			}
		}
		for _, w := range repo.got.Wants {
			if w != idA {
				t.Fatalf("served a non-tip want %q", w)
			}
		}
	})
}

// endlessLines yields "0009peel\n" lines forever: a request that never ends.
type endlessLines struct {
	n int64
}

func (e *endlessLines) Read(p []byte) (int, error) {
	const line = "0009peel\n"
	for i := range p {
		p[i] = line[(e.n+int64(i))%int64(len(line))]
	}
	e.n += int64(len(p))
	return len(p), nil
}

// TestBombEndlessRequestIsRefused: a request that never ends is cut off by
// the byte/line limits, reading at most limit + one buffer, under a hard
// memory limit.
func TestBombEndlessRequestIsRefused(t *testing.T) {
	old := debug.SetMemoryLimit(64 << 20)
	defer debug.SetMemoryLimit(old)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	head := strings.NewReader("0014command=ls-refs\n0001")
	src := &endlessLines{}
	s := &gitproto.Server{Repo: &fakeRepo{}, Limits: gitproto.Limits{MaxRequestBytes: 1 << 20}}
	var out bytes.Buffer
	err := s.ServeRequest(context.Background(), io.MultiReader(head, src), &out)
	var pe *gitproto.ProtocolError
	if !errors.As(err, &pe) || !strings.Contains(pe.Msg, "exceeds") {
		t.Fatalf("want a limit refusal, got %v", err)
	}
	if src.n > 1<<20+128<<10 {
		t.Fatalf("read %d bytes from an endless request", src.n)
	}
	runtime.ReadMemStats(&after)
	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > 32<<20 {
		t.Fatalf("heap grew by %d bytes", grown)
	}
}

// TestBombOversizePacketIsRefused: an oversize length header is refused at
// the server, after four bytes.
func TestBombOversizePacketIsRefused(t *testing.T) {
	src := &endlessLines{}
	s := &gitproto.Server{Repo: &fakeRepo{}}
	var out bytes.Buffer
	err := s.ServeRequest(context.Background(), io.MultiReader(strings.NewReader("fff1"), src), &out)
	var pe *gitproto.ProtocolError
	if !errors.As(err, &pe) || !strings.Contains(pe.Msg, "65520") {
		t.Fatalf("got %v", err)
	}
	if src.n > 64<<10 {
		t.Fatalf("read %d bytes past the oversize header", src.n)
	}
	if !strings.Contains(out.String(), "ERR malformed pkt-line") {
		t.Fatalf("client not told: %q", out.String())
	}
}

// TestBombManyWantsIsRefused: a million want lines are refused by the
// default limits before the repository is consulted.
func TestBombManyWantsIsRefused(t *testing.T) {
	var b bytes.Buffer
	w := pktline.NewWriter(&b)
	_ = w.Text("command=fetch")
	_ = w.Delim()
	for i := 0; i < 60_000; i++ {
		_ = w.Text("want " + idA[:39] + fmt.Sprintf("%040x", i))
	}
	_ = w.Flush()
	repo := &fakeRepo{refsErr: errors.New("repository must not be consulted")}
	_, err := serveOne(t, &gitproto.Server{Repo: repo}, b.Bytes())
	var pe *gitproto.ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("got %v", err)
	}
}
