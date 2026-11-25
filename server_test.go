package gitproto_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-vcs/gitproto"
	"github.com/go-vcs/gitproto/pktline"
)

func newServer(r gitproto.Repository) *gitproto.Server {
	return &gitproto.Server{Repo: r}
}

func TestAdvertise(t *testing.T) {
	var b bytes.Buffer
	if err := newServer(nil).Advertise(&b); err != nil {
		t.Fatal(err)
	}
	want := "000eversion 2\n" + "0013agent=gitproto\n" + "0013ls-refs=unborn\n" + "000afetch\n" + "0017object-format=sha1\n" + "0000"
	if b.String() != want {
		t.Fatalf("got %q\nwant %q", b.String(), want)
	}
	b.Reset()
	s := &gitproto.Server{Agent: "forge/1"}
	_ = s.Advertise(&b)
	if !strings.Contains(b.String(), "agent=forge/1\n") {
		t.Fatalf("custom agent missing: %q", b.String())
	}
}

// TestLsRefsMatchesGit compares ls-refs byte for byte with git upload-pack.
func TestLsRefsMatchesGit(t *testing.T) {
	f := newFixture(t)
	empty := filepath.Join(t.TempDir(), "empty.git")
	git(t, filepath.Dir(empty), "init", "-q", "--bare", "-b", "main", empty)
	detached := filepath.Join(t.TempDir(), "det.git")
	git(t, filepath.Dir(detached), "clone", "-q", "--bare", f.Work, detached)
	write(t, filepath.Join(detached, "HEAD"), f.Topic+"\n")
	atTag := filepath.Join(t.TempDir(), "tag.git")
	git(t, filepath.Dir(atTag), "clone", "-q", "--bare", f.Work, atTag)
	write(t, filepath.Join(atTag, "HEAD"), f.TagTag+"\n")
	withSym := filepath.Join(t.TempDir(), "sym.git")
	git(t, filepath.Dir(withSym), "clone", "-q", "--bare", f.Work, withSym)
	git(t, withSym, "symbolic-ref", "refs/heads/alias", "refs/heads/topic")

	argsets := [][]string{
		nil,
		{"peel"},
		{"symrefs"},
		{"peel", "symrefs"},
		{"peel", "symrefs", "unborn"},
		{"symrefs", "unborn"},
		{"unborn"},
		{"peel", "symrefs", "ref-prefix HEAD", "ref-prefix refs/tags/"},
		{"peel", "ref-prefix refs/heads/m"},
		{"symrefs", "ref-prefix refs/nothing/"},
	}
	for name, dir := range map[string]string{"normal": f.Bare, "empty": empty, "detached": detached, "head-at-tag": atTag, "symref-ref": withSym} {
		for _, args := range argsets {
			req := request("ls-refs", []string{"agent=git/2.47", "object-format=sha1"}, args...)
			got, err := serveOne(t, newServer(&gitproto.GitRepo{Dir: dir}), req)
			if err != nil {
				t.Fatalf("%s %v: %v", name, args, err)
			}
			want := gitUploadPack(t, dir, req)
			if !bytes.Equal(got, want) {
				t.Errorf("%s %v:\n got %q\nwant %q", name, args, got, want)
			}
		}
	}
}

func TestLsRefsPeelsNestedTags(t *testing.T) {
	f := newFixture(t)
	out, err := serveOne(t, newServer(f.repo()), request("ls-refs", nil, "peel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), f.TagTag+" refs/tags/v1v1\n") && !strings.Contains(string(out), " refs/tags/v1v1 peeled:"+f.TagTarget+"\n") {
		t.Fatalf("v1v1 not peeled to the commit: %q", out)
	}
}

func fetchReq(args ...string) []byte {
	return request("fetch", []string{"agent=git/2.47", "object-format=sha1"}, args...)
}

func TestFetchClone(t *testing.T) {
	f := newFixture(t)
	resp, err := serveOne(t, newServer(f.repo()), fetchReq(
		"thin-pack", "no-progress", "ofs-delta", "include-tag",
		"want "+f.Main, "want "+f.Topic, "want "+f.Tag, "want "+f.TagTag, "done"))
	if err != nil {
		t.Fatal(err)
	}
	pre, pack, _ := splitPackResponse(t, resp)
	if len(pre) != 1 || pre[0] != "packfile" {
		t.Fatalf("sections before the pack: %q (acknowledgments must be omitted after done)", pre)
	}
	dst := indexPack(t, pack, false, "")
	for _, id := range []string{f.Main, f.Topic, f.Tag, f.TagTag} {
		git(t, dst, "cat-file", "-e", id)
	}
	git(t, dst, "update-ref", "refs/heads/main", f.Main)
	git(t, dst, "fsck", "--strict")
}

func TestFetchIncrementalThin(t *testing.T) {
	f := newFixture(t)
	old := f.Main
	write(t, filepath.Join(f.Work, "f.txt"), strings.Repeat("three\n", 149)+"changed\n")
	git(t, f.Work, "commit", "-q", "-a", "-m", "four")
	git(t, f.Work, "push", "-q", f.Bare, "main")
	newTip := git(t, f.Work, "rev-parse", "HEAD")

	full, err := serveOne(t, newServer(f.repo()), fetchReq("want "+newTip, "done"))
	if err != nil {
		t.Fatal(err)
	}
	_, fullPack, _ := splitPackResponse(t, full)

	resp, err := serveOne(t, newServer(f.repo()), fetchReq("thin-pack", "ofs-delta", "want "+newTip, "have "+old, "done"))
	if err != nil {
		t.Fatal(err)
	}
	_, pack, _ := splitPackResponse(t, resp)
	if len(pack) >= len(fullPack) {
		t.Errorf("incremental pack (%d bytes) not smaller than full pack (%d bytes)", len(pack), len(fullPack))
	}
	// A client that has `old` completes the thin pack.
	basis := filepath.Join(t.TempDir(), "basis.git")
	git(t, filepath.Dir(basis), "init", "-q", "--bare", basis)
	git(t, f.Work, "push", "-q", basis, old+":refs/heads/main")
	dst := indexPack(t, pack, true, basis)
	git(t, dst, "cat-file", "-e", newTip)
	git(t, dst, "update-ref", "refs/heads/main", newTip)
	git(t, dst, "fsck", "--strict")
}

func TestFetchIncludeTag(t *testing.T) {
	f := newFixture(t)
	has := func(args ...string) bool {
		resp, err := serveOne(t, newServer(f.repo()), fetchReq(append(args, "want "+f.Main, "done")...))
		if err != nil {
			t.Fatal(err)
		}
		_, pack, _ := splitPackResponse(t, resp)
		dst := indexPack(t, pack, false, "")
		c := git2(dst, "cat-file", "-e", f.Tag)
		return c == nil
	}
	if has() {
		t.Error("tag object sent without include-tag")
	}
	if !has("include-tag") {
		t.Error("tag object missing with include-tag")
	}
}

const nak = "0014acknowledgments\n0008NAK\n0000"

func TestFetchNegotiation(t *testing.T) {
	f := newFixture(t)
	s := newServer(f.repo())
	parent := git(t, f.Work, "rev-parse", "main~1")
	unknown := strings.Repeat("ab", 20)

	// No common object: NAK, flush, no pack.
	resp, err := serveOne(t, s, fetchReq("want "+f.Main, "have "+unknown))
	if err != nil {
		t.Fatal(err)
	}
	if want := nak; string(resp) != want {
		t.Fatalf("got %q want %q", resp, want)
	}
	// No haves at all and no done: NAK as well.
	resp, _ = serveOne(t, s, fetchReq("want "+f.Main))
	if string(resp) != nak {
		t.Fatalf("got %q", resp)
	}
	// One common (and one unknown): ACK only the common, ready, pack.
	resp, err = serveOne(t, s, fetchReq("want "+f.Main, "have "+unknown, "have "+parent, "have "+parent))
	if err != nil {
		t.Fatal(err)
	}
	pre, pack, _ := splitPackResponse(t, resp)
	want := []string{"acknowledgments", "ACK " + parent, "ready", "<delim>", "packfile"}
	if strings.Join(pre, "|") != strings.Join(want, "|") {
		t.Fatalf("sections %q want %q", pre, want)
	}
	if len(pack) == 0 {
		t.Fatal("empty pack")
	}
	// A blob id is not a common commit.
	blob := git(t, f.Work, "rev-parse", "main:f.txt")
	resp, _ = serveOne(t, s, fetchReq("want "+f.Main, "have "+blob))
	if string(resp) != nak {
		t.Fatalf("blob counted as common: %q", resp)
	}
}

func TestFetchNoWants(t *testing.T) {
	f := newFixture(t)
	resp, err := serveOne(t, newServer(f.repo()), fetchReq("done"))
	if err != nil || string(resp) != "0000" {
		t.Fatalf("%q %v", resp, err)
	}
}

func TestWantMustBeATip(t *testing.T) {
	f := newFixture(t)
	parent := git(t, f.Work, "rev-parse", "main~1")
	resp, err := serveOne(t, newServer(f.repo()), fetchReq("want "+parent, "done"))
	assertRefused(t, resp, err, "not our ref "+parent)
	// An id that does not exist at all is refused the same way.
	resp, err = serveOne(t, newServer(f.repo()), fetchReq("want "+strings.Repeat("0", 40), "done"))
	assertRefused(t, resp, err, "not our ref")
	// The peeled object of an annotated tag is an advertised tip.
	if _, err := serveOne(t, newServer(f.repo()), fetchReq("want "+f.TagTarget, "done")); err != nil {
		t.Fatal(err)
	}
}

func assertRefused(t testing.TB, resp []byte, err error, contains string) {
	t.Helper()
	var pe *gitproto.ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("want *ProtocolError, got %v (output %q)", err, resp)
	}
	if !strings.Contains(pe.Msg, contains) {
		t.Fatalf("refusal %q does not contain %q", pe.Msg, contains)
	}
	r := pktline.NewReader(bytes.NewReader(resp))
	var last string
	for {
		p, err := r.Read()
		if err != nil {
			break
		}
		last = string(pktline.Text(p.Data))
	}
	if last != "ERR "+pe.Msg {
		t.Fatalf("client was sent %q, want ERR %s", last, pe.Msg)
	}
}

func TestRefusals(t *testing.T) {
	f := newFixture(t)
	oid := f.Main
	cases := map[string]struct {
		req  []byte
		want string
	}{
		"unknown command":      {request("push", nil), "unsupported command"},
		"bundle-uri":           {request("bundle-uri", nil), "unsupported command"},
		"object-info":          {request("object-info", nil, "size", "oid "+oid), "unsupported command"},
		"server-option":        {request("ls-refs", []string{"server-option=x"}), "unsupported capability"},
		"unknown capability":   {request("ls-refs", []string{"frobnicate"}), "unsupported capability"},
		"sha256":               {request("ls-refs", []string{"object-format=sha256"}), "unsupported object-format"},
		"agent with space":     {request("ls-refs", []string{"agent=a b"}), "agent contains"},
		"agent empty":          {request("ls-refs", []string{"agent="}), "empty agent"},
		"agent twice":          {request("ls-refs", []string{"agent=a", "agent=b"}), "duplicate agent"},
		"object-format twice":  {request("ls-refs", []string{"object-format=sha1", "object-format=sha1"}), "duplicate object-format"},
		"bad capability name":  {request("ls-refs", []string{"a b=c"}), "invalid capability"},
		"bad command name":     {[]byte("0010command=a b\n0000"), "invalid command name"},
		"empty command name":   {[]byte("000dcommand=\n0000"), "invalid command name"},
		"empty capability":     {[]byte("0014command=ls-refs\n0007=x\n0000"), "invalid capability"},
		"no command=":          {[]byte("0008ls-r\n0000"), "does not start with command="},
		"delim first":          {[]byte("0001"), "unexpected delim packet"},
		"response-end first":   {[]byte("0002"), "unexpected response-end packet at start"},
		"response-end inside":  {[]byte("0014command=ls-refs\n0002"), "unexpected response-end packet in request"},
		"second delim":         {[]byte("0014command=ls-refs\n000100010000"), "second delim-pkt"},
		"truncated":            {[]byte("0014command=ls-r"), "request truncated"},
		"truncated before end": {[]byte("0014command=ls-refs\n"), "request truncated"},
		"bad length":           {[]byte("zzzzcommand=ls-refs\n0000"), "malformed pkt-line"},
		"overlong":             {[]byte("ffff"), "malformed pkt-line"},
		"reserved 0003":        {[]byte("0003"), "malformed pkt-line"},
		"NUL":                  {[]byte("0015command=ls-refs\x00\n0000"), "NUL byte"},
		"NUL in args":          {[]byte("0014command=ls-refs\n00010009a\x00b\n0000"), "NUL byte"},
		"ls-refs unknown arg":  {request("ls-refs", nil, "frob"), "unsupported ls-refs argument"},
		"ls-refs peel value":   {request("ls-refs", nil, "peel x"), "unsupported ls-refs argument"},
		"fetch unknown arg":    {fetchReq("frob"), "unknown fetch argument"},
		"want no value":        {fetchReq("want"), "invalid object id"},
		"want short":           {fetchReq("want abc"), "invalid object id"},
		"want upper":           {fetchReq("want " + strings.ToUpper(oid)), "invalid object id"},
		"want 64 hex":          {fetchReq("want " + oid + oid[:24]), "invalid object id"},
		"have bad":             {fetchReq("want "+oid, "have zz"), "invalid object id"},
		"done with value":      {fetchReq("want "+oid, "done x"), "takes no value"},
		"thin-pack with value": {fetchReq("thin-pack x"), "takes no value"},
	}
	for _, a := range []string{
		"shallow " + oid, "deepen 1", "deepen-relative", "deepen-since 1", "deepen-not refs/heads/x",
		"filter blob:none", "want-ref refs/heads/main", "sideband-all", "packfile-uris https", "wait-for-done",
	} {
		name, _, _ := strings.Cut(a, " ")
		cases["unsupported "+name] = struct {
			req  []byte
			want string
		}{fetchReq("want "+oid, a, "done"), "unsupported fetch argument \"" + name + "\""}
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			resp, err := serveOne(t, newServer(f.repo()), tc.req)
			assertRefused(t, resp, err, tc.want)
		})
	}
}

// A refusal echoes at most 64 bytes of what the client sent.
func TestRefusalClipsEchoedInput(t *testing.T) {
	for n, wantClip := range map[int]bool{63: false, 64: false, 65: true, 1000: true} {
		resp, err := serveOne(t, newServer(nil), request("ls-refs", []string{"object-format=" + strings.Repeat("x", n)}))
		assertRefused(t, resp, err, "unsupported object-format")
		var pe *gitproto.ProtocolError
		errors.As(err, &pe)
		if got := strings.Contains(pe.Msg, "..."); got != wantClip {
			t.Errorf("%d bytes: clipped=%v, want %v: %q", n, got, wantClip, pe.Msg)
		}
		if len(pe.Msg) > 200 {
			t.Errorf("%d bytes: refusal is %d bytes long", n, len(pe.Msg))
		}
	}
}

// A write failure during the acknowledgments must stop the exchange before
// the repository is asked to generate a pack nobody can receive.
func TestNoPackIsGeneratedForADeadClient(t *testing.T) {
	var common []string
	for range 3000 {
		common = append(common, idB)
	}
	repo := &fakeRepo{refs: []gitproto.Ref{{Name: "HEAD", ID: idA}}, common: common, pack: []byte("P")}
	err := newServer(repo).ServeRequest(context.Background(),
		bytes.NewReader(fetchReq("want "+idA, "have "+idB)), &failWriter{n: 0})
	if !errors.Is(err, errWrite) {
		t.Fatalf("got %v", err)
	}
	if len(repo.got.Wants) != 0 {
		t.Fatal("WritePack was called after the acknowledgments failed")
	}
}

func TestLimits(t *testing.T) {
	f := newFixture(t)
	mk := func(l gitproto.Limits) *gitproto.Server {
		return &gitproto.Server{Repo: f.repo(), Limits: l}
	}
	other := git(t, f.Work, "rev-parse", "main~1")
	for name, tc := range map[string]struct {
		lim  gitproto.Limits
		req  []byte
		want string
	}{
		"wants":    {gitproto.Limits{MaxWants: 1}, fetchReq("want "+f.Main, "want "+f.Topic, "done"), "more than 1 want"},
		"haves":    {gitproto.Limits{MaxHaves: 1}, fetchReq("want "+f.Main, "have "+f.Main, "have "+other, "done"), "more than 1 have"},
		"prefixes": {gitproto.Limits{MaxRefPrefixes: 1}, request("ls-refs", nil, "ref-prefix a", "ref-prefix b"), "more than 1 ref-prefix"},
		"lines":    {gitproto.Limits{MaxRequestLines: 3}, request("ls-refs", nil, "peel", "symrefs"), "exceeds 3 lines"},
		"bytes":    {gitproto.Limits{MaxRequestBytes: 30}, request("ls-refs", []string{"agent=git/2.47"}, "peel"), "exceeds 30 bytes"},
	} {
		resp, err := serveOne(t, mk(tc.lim), tc.req)
		t.Run(name, func(t *testing.T) { assertRefused(t, resp, err, tc.want) })
	}
	// Exactly at the limit is accepted.
	if _, err := serveOne(t, mk(gitproto.Limits{MaxWants: 2, MaxHaves: 1, MaxRefPrefixes: 1, MaxRequestLines: 100}), fetchReq("want "+f.Main, "want "+f.Topic, "have "+other, "done")); err != nil {
		t.Fatalf("at-limit request refused: %v", err)
	}
	if _, err := serveOne(t, mk(gitproto.Limits{MaxRefPrefixes: 1}), request("ls-refs", nil, "ref-prefix a")); err != nil {
		t.Fatalf("at-limit prefix refused: %v", err)
	}
	if err := (gitproto.Limits{MaxWants: -1}).Validate(); err == nil {
		t.Error("negative limit accepted")
	}
	for _, l := range []gitproto.Limits{{MaxRequestBytes: -1}, {MaxRequestLines: -1}, {MaxHaves: -1}, {MaxRefPrefixes: -1}} {
		if l.Validate() == nil {
			t.Errorf("%+v accepted", l)
		}
	}
	s := &gitproto.Server{Repo: f.repo(), Limits: gitproto.Limits{MaxWants: -1}}
	if err := s.Serve(context.Background(), strings.NewReader(""), io.Discard); err == nil {
		t.Error("Serve accepted a negative limit")
	}
	if _, err := serveOne(t, s, request("ls-refs", nil)); err == nil {
		t.Error("ServeRequest accepted a negative limit")
	}
}

func TestSession(t *testing.T) {
	f := newFixture(t)
	var in bytes.Buffer
	in.Write(request("ls-refs", nil, "peel"))
	in.Write(fetchReq("want "+f.Main, "done"))
	in.WriteString("0000") // the empty request ends the session
	in.Write(request("ls-refs", nil))
	var out bytes.Buffer
	if err := newServer(f.repo()).Serve(context.Background(), &in, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "000eversion 2\n") {
		t.Fatalf("no advertisement: %q", out.String()[:20])
	}
	if n := strings.Count(out.String(), "refs/heads/main"); n != 1 {
		t.Fatalf("the request after the empty request was served (%d)", n)
	}
	if !strings.Contains(out.String(), "packfile\n") {
		t.Fatal("no packfile")
	}
	// A clean EOF between requests ends the session without error, and
	// without an ERR line.
	out.Reset()
	if err := newServer(f.repo()).Serve(context.Background(), bytes.NewReader(request("ls-refs", nil)), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "ERR") {
		t.Fatalf("clean end of session sent an error: %q", out.String())
	}
	// A refused request ends the session with the refusal.
	out.Reset()
	err := newServer(f.repo()).Serve(context.Background(), bytes.NewReader(request("push", nil)), &out)
	var pe *gitproto.ProtocolError
	if !errors.As(err, &pe) || !strings.Contains(out.String(), "ERR unsupported command") {
		t.Fatalf("%v %q", err, out.String())
	}
}

func TestServeRequestEmpty(t *testing.T) {
	s := newServer(nil)
	if err := s.ServeRequest(context.Background(), strings.NewReader("0000"), io.Discard); err != io.EOF {
		t.Fatalf("empty request: %v", err)
	}
	if err := s.ServeRequest(context.Background(), strings.NewReader(""), io.Discard); err != io.EOF {
		t.Fatalf("no request: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Failure injection with a fake repository.

type fakeRepo struct {
	refs      []gitproto.Ref
	common    []string
	pack      []byte
	refsErr   error
	commonErr error
	packErr   error
	got       gitproto.PackRequest
}

func (r *fakeRepo) Refs(context.Context) ([]gitproto.Ref, error) { return r.refs, r.refsErr }
func (r *fakeRepo) CommonCommits(context.Context, []string) ([]string, error) {
	return r.common, r.commonErr
}
func (r *fakeRepo) WritePack(_ context.Context, req gitproto.PackRequest, w io.Writer) error {
	r.got = req
	if _, err := w.Write(r.pack); err != nil {
		return err
	}
	return r.packErr
}

var (
	idA = strings.Repeat("a", 40)
	idB = strings.Repeat("b", 40)
)

func TestRepositoryErrorsAreNotLeaked(t *testing.T) {
	secret := errors.New("/srv/secret/path exploded")
	for name, tc := range map[string]struct {
		repo *fakeRepo
		req  []byte
	}{
		"refs for ls-refs": {&fakeRepo{refsErr: secret}, request("ls-refs", nil)},
		"refs for fetch":   {&fakeRepo{refsErr: secret}, fetchReq("want "+idA, "done")},
		"common":           {&fakeRepo{refs: []gitproto.Ref{{Name: "HEAD", ID: idA}}, commonErr: secret}, fetchReq("want "+idA, "have "+idB)},
	} {
		resp, err := serveOne(t, newServer(tc.repo), tc.req)
		if !errors.Is(err, secret) {
			t.Errorf("%s: err = %v", name, err)
		}
		if strings.Contains(string(resp), "secret") || !strings.Contains(string(resp), "ERR internal error") {
			t.Errorf("%s: client sees %q", name, resp)
		}
	}
}

func TestPackFailureIsReportedOnSideband3(t *testing.T) {
	secret := errors.New("/srv/secret/path exploded")
	repo := &fakeRepo{refs: []gitproto.Ref{{Name: "HEAD", ID: idA}}, pack: []byte("PACKpartial"), packErr: secret}
	resp, err := serveOne(t, newServer(repo), fetchReq("want "+idA, "done"))
	if !errors.Is(err, secret) {
		t.Fatal(err)
	}
	if !bytes.Contains(resp, []byte("\x03ERR pack generation failed")) || bytes.Contains(resp, []byte("secret")) {
		t.Fatalf("%q", resp)
	}
	if !bytes.HasSuffix(resp, []byte("ERR internal error\n")) {
		// the stream was aborted: no flush-pkt follows the error
		t.Logf("tail: %q", resp[max(0, len(resp)-40):])
	}
}

func TestFetchPassesOptionsToTheRepository(t *testing.T) {
	repo := &fakeRepo{refs: []gitproto.Ref{{Name: "HEAD", ID: idA}}, common: []string{idB}, pack: []byte("P")}
	if _, err := serveOne(t, newServer(repo), fetchReq("thin-pack", "ofs-delta", "include-tag", "no-progress", "want "+idA, "have "+idB, "done")); err != nil {
		t.Fatal(err)
	}
	want := gitproto.PackRequest{Wants: []string{idA}, Common: []string{idB}, Thin: true, OfsDelta: true, IncludeTag: true}
	g := repo.got
	if strings.Join(g.Wants, ",") != idA || strings.Join(g.Common, ",") != idB || !g.Thin || !g.OfsDelta || !g.IncludeTag {
		t.Fatalf("got %+v want %+v", g, want)
	}
	if _, err := serveOne(t, newServer(repo), fetchReq("want "+idA, "done")); err != nil {
		t.Fatal(err)
	}
	if g := repo.got; g.Thin || g.OfsDelta || g.IncludeTag {
		t.Fatalf("options set without being asked: %+v", g)
	}
}

// failWriter fails after n bytes.
type failWriter struct{ n int }

var errWrite = errors.New("write failed")

func (w *failWriter) Write(p []byte) (int, error) {
	if w.n <= 0 {
		return 0, errWrite
	}
	if len(p) > w.n {
		k := w.n
		w.n = 0
		return k, errWrite
	}
	w.n -= len(p)
	return len(p), nil
}

// TestEveryWriteFailureIsReturned cuts the output at every byte offset of a
// full exchange and requires an error each time.
func TestEveryWriteFailureIsReturned(t *testing.T) {
	refs := []gitproto.Ref{
		{Name: "HEAD", ID: idA, Target: "refs/heads/main"},
		{Name: "refs/heads/main", ID: idA},
		{Name: "refs/tags/t", ID: idB, Peeled: idA},
	}
	repo := &fakeRepo{refs: refs, common: []string{idB}, pack: bytes.Repeat([]byte("x"), 70000)}
	for name, req := range map[string][]byte{
		"ls-refs":     request("ls-refs", nil, "peel", "symrefs"),
		"unborn":      request("ls-refs", nil, "unborn"),
		"fetch done":  fetchReq("want "+idA, "done"),
		"fetch ready": fetchReq("want "+idA, "have "+idB),
		"fetch nak":   fetchReq("want " + idA),
		"fetch none":  fetchReq("done"),
	} {
		var full bytes.Buffer
		if err := newServer(repo).ServeRequest(context.Background(), bytes.NewReader(req), &full); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// bufio makes writes coarse, so test through a 1-byte-granular
		// path: Advertise writes straight to the writer, the rest goes
		// through a 64 KiB buffer, flushed once at the end or as it fills.
		for _, cut := range []int{0, 1, 5, 20, 100, 65536, 65600, 66000} {
			if cut >= full.Len() {
				continue
			}
			err := newServer(repo).ServeRequest(context.Background(), bytes.NewReader(req), &failWriter{n: cut})
			if err == nil {
				t.Errorf("%s: write failure at byte %d went unreported", name, cut)
			}
		}
	}
	var out bytes.Buffer
	_ = newServer(repo).Advertise(&out)
	for cut := 0; cut < out.Len(); cut++ {
		if err := newServer(repo).Advertise(&failWriter{n: cut}); err == nil {
			t.Errorf("Advertise: failure at byte %d unreported", cut)
		}
	}
	err := newServer(repo).Serve(context.Background(), strings.NewReader(""), &failWriter{n: 0})
	if err == nil {
		t.Error("Serve: advertisement write failure unreported")
	}
	err = newServer(repo).Serve(context.Background(), strings.NewReader(""), &failWriter{n: out.Len() - 1})
	if err == nil {
		t.Error("Serve: advertisement flush failure unreported")
	}
}

// failAfter lets n packets through, then fails. It sees whole pkt-lines, since
// Writer makes one Write call per packet; used to hit each write in order.
type countWriter struct {
	n   int
	buf bytes.Buffer
}

func (w *countWriter) Write(p []byte) (int, error) {
	if w.n == 0 {
		return 0, errWrite
	}
	w.n--
	return w.buf.Write(p)
}

func TestEveryPacketWriteFailureIsReturned(t *testing.T) {
	// Drive the handlers through Serve with an unbuffered view of the output
	// by using a pack large enough to flush the buffer repeatedly.
	repo := &fakeRepo{
		refs:   []gitproto.Ref{{Name: "HEAD", ID: idA, Target: "refs/heads/main"}, {Name: "refs/heads/main", ID: idA}, {Name: "refs/tags/t", ID: idB, Peeled: idA}},
		common: []string{idA, idB},
		pack:   bytes.Repeat([]byte("x"), 300_000),
	}
	for name, req := range map[string][]byte{
		"ls-refs": request("ls-refs", nil, "peel", "symrefs", "unborn"),
		"ready":   fetchReq("want "+idA, "have "+idA, "have "+idB),
		"done":    fetchReq("want "+idA, "done"),
	} {
		var full countWriter
		full.n = 1 << 30
		if err := newServer(repo).ServeRequest(context.Background(), bytes.NewReader(req), &full); err != nil {
			t.Fatal(err)
		}
		for n := 0; n < full.buf.Len(); n += 997 {
			if err := newServer(repo).ServeRequest(context.Background(), bytes.NewReader(req), &failWriter{n: n}); err == nil {
				t.Errorf("%s: failure after %d bytes unreported", name, n)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// GitRepo

func TestGitRepoRefusesBadInput(t *testing.T) {
	f := newFixture(t)
	r := f.repo()
	ctx := context.Background()
	if _, err := r.CommonCommits(ctx, []string{"--all"}); err == nil {
		t.Error("CommonCommits accepted a non-oid")
	}
	if got, err := r.CommonCommits(ctx, nil); err != nil || got != nil {
		t.Errorf("CommonCommits(nil) = %v %v", got, err)
	}
	if err := r.WritePack(ctx, gitproto.PackRequest{Wants: []string{"--all"}}, io.Discard); err == nil {
		t.Error("WritePack accepted a non-oid want")
	}
	if err := r.WritePack(ctx, gitproto.PackRequest{Wants: []string{f.Main}, Common: []string{"--all"}}, io.Discard); err == nil {
		t.Error("WritePack accepted a non-oid have")
	}
	for _, id := range []string{"", "abc", strings.Repeat("g", 40), strings.Repeat("A", 40), strings.Repeat("a", 41)} {
		if gitproto.ValidOID(id) {
			t.Errorf("ValidOID(%q)", id)
		}
	}
	if !gitproto.ValidOID(f.Main) {
		t.Error("ValidOID rejects a real id")
	}
}

func TestGitRepoErrors(t *testing.T) {
	ctx := context.Background()
	notRepo := &gitproto.GitRepo{Dir: t.TempDir()}
	if _, err := notRepo.Refs(ctx); err == nil {
		t.Error("Refs on a non-repository succeeded")
	}
	if _, err := notRepo.CommonCommits(ctx, []string{idA}); err == nil {
		t.Error("CommonCommits on a non-repository succeeded")
	}
	if err := notRepo.WritePack(ctx, gitproto.PackRequest{Wants: []string{idA}}, io.Discard); err == nil {
		t.Error("WritePack on a non-repository succeeded")
	}
	missing := &gitproto.GitRepo{Dir: t.TempDir(), Git: filepath.Join(t.TempDir(), "no-such-git")}
	if _, err := missing.Refs(ctx); err == nil {
		t.Error("Refs without a git binary succeeded")
	}
	if err := missing.WritePack(ctx, gitproto.PackRequest{Wants: []string{idA}}, io.Discard); err == nil {
		t.Error("WritePack without a git binary succeeded")
	}
	f := newFixture(t)
	// Wants that do not exist make pack-objects fail; the error is bounded and
	// carries git's stderr for the operator.
	err := f.repo().WritePack(ctx, gitproto.PackRequest{Wants: []string{idA}}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "pack-objects") {
		t.Errorf("WritePack of a missing object: %v", err)
	}
	// A consumer that stops reading ends the pack without hanging.
	if err := f.repo().WritePack(ctx, gitproto.PackRequest{Wants: []string{f.Main}}, &failWriter{}); !errors.Is(err, errWrite) {
		t.Errorf("failing consumer: %v", err)
	}
	// A cancelled context stops git.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.repo().Refs(cctx); err == nil {
		t.Error("Refs with a cancelled context succeeded")
	}
}

func TestGitRepoPackThreads(t *testing.T) {
	f := newFixture(t)
	r := &gitproto.GitRepo{Dir: f.Bare, PackThreads: 1}
	var b bytes.Buffer
	if err := r.WritePack(context.Background(), gitproto.PackRequest{Wants: []string{f.Main}}, &b); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(b.Bytes(), []byte("PACK")) {
		t.Fatalf("not a pack: %q", b.Bytes()[:8])
	}
}

func TestGitRepoIgnoresUserConfig(t *testing.T) {
	// A hostile global config must not reach git: point GIT_CONFIG_GLOBAL
	// at a file that makes every command fail, and serve anyway.
	f := newFixture(t)
	bad := filepath.Join(t.TempDir(), "gitconfig")
	write(t, bad, "[core\nbroken")
	t.Setenv("GIT_CONFIG_GLOBAL", bad)
	t.Setenv("GIT_DIR", "/nonexistent")
	if _, err := f.repo().Refs(context.Background()); err != nil {
		t.Fatalf("ambient git environment leaked in: %v", err)
	}
	_ = os.Remove(bad)
}

// git2 runs git and returns its error (the fixture helper fails the test).
func git2(dir string, args ...string) error {
	c := exec.Command("git", args...)
	c.Dir = dir
	return c.Run()
}
