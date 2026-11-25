package gitproto_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-vcs/gitproto"
	"github.com/go-vcs/gitproto/pktline"
)

// git runs the real git binary (the oracle) in dir and returns trimmed stdout.
func git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.org", "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.org", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

func write(t testing.TB, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture is a work tree with history, branches, a lightweight tag, an
// annotated tag and a tag of a tag, plus a bare clone of it to serve.
type fixture struct {
	Work, Bare string
	Main       string // commit at refs/heads/main
	Topic      string
	Tag        string // object id of annotated tag v1
	TagTarget  string // commit v1 points at
	TagTag     string // object id of the tag-of-tag
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{Work: filepath.Join(root, "work"), Bare: filepath.Join(root, "bare.git")}
	if err := os.Mkdir(f.Work, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, f.Work, "init", "-q", "-b", "main")
	for i, s := range []string{"one", "two", "three"} {
		write(t, filepath.Join(f.Work, "f.txt"), strings.Repeat(s+"\n", 50*(i+1)))
		git(t, f.Work, "add", "f.txt")
		git(t, f.Work, "commit", "-q", "-m", s)
	}
	f.TagTarget = git(t, f.Work, "rev-parse", "HEAD")
	git(t, f.Work, "tag", "light")
	git(t, f.Work, "tag", "-a", "-m", "v1", "v1")
	git(t, f.Work, "tag", "-a", "-m", "v1 of v1", "v1v1", "v1")
	git(t, f.Work, "checkout", "-q", "-b", "topic")
	write(t, filepath.Join(f.Work, "g.txt"), "topic\n")
	git(t, f.Work, "add", "g.txt")
	git(t, f.Work, "commit", "-q", "-m", "topic")
	f.Topic = git(t, f.Work, "rev-parse", "HEAD")
	git(t, f.Work, "checkout", "-q", "main")
	f.Main = git(t, f.Work, "rev-parse", "HEAD")
	f.Tag = git(t, f.Work, "rev-parse", "v1")
	f.TagTag = git(t, f.Work, "rev-parse", "v1v1")
	git(t, root, "clone", "-q", "--bare", f.Work, f.Bare)
	return f
}

func (f *fixture) repo() *gitproto.GitRepo { return &gitproto.GitRepo{Dir: f.Bare} }

// request builds a v2 command request.
func request(cmd string, caps []string, args ...string) []byte {
	var b bytes.Buffer
	w := pktline.NewWriter(&b)
	_ = w.Text("command=" + cmd)
	for _, c := range caps {
		_ = w.Text(c)
	}
	if len(args) > 0 {
		_ = w.Delim()
		for _, a := range args {
			_ = w.Text(a)
		}
	}
	_ = w.Flush()
	return b.Bytes()
}

// serveOne runs one request through ServeRequest.
func serveOne(t testing.TB, s *gitproto.Server, req []byte) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	err := s.ServeRequest(context.Background(), bytes.NewReader(req), &out)
	return out.Bytes(), err
}

// gitUploadPack runs `git upload-pack --stateless-rpc` (protocol v2) on dir
// with req: the oracle for the server's answers.
func gitUploadPack(t testing.TB, dir string, req []byte) []byte {
	t.Helper()
	c := exec.Command("git", "upload-pack", "--stateless-rpc", dir)
	c.Env = append(os.Environ(), "GIT_PROTOCOL=version=2", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	c.Stdin = bytes.NewReader(req)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		t.Fatalf("git upload-pack: %v\n%s", err, errb.String())
	}
	return out.Bytes()
}

// splitPackResponse parses a fetch response that carries a packfile section
// and returns the lines before the packfile header and the de-multiplexed
// pack bytes.
func splitPackResponse(t testing.TB, resp []byte) (pre []string, pack []byte, progress []byte) {
	t.Helper()
	r := pktline.NewReader(bytes.NewReader(resp))
	inPack := false
	for {
		p, err := r.Read()
		if err == io.EOF {
			t.Fatalf("response ended without a flush-pkt: %q", resp)
		}
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case p.Kind == pktline.Flush:
			if !inPack {
				return pre, nil, nil
			}
			return pre, pack, progress
		case p.Kind == pktline.Delim:
			pre = append(pre, "<delim>")
		case !inPack:
			s := string(pktline.Text(p.Data))
			pre = append(pre, s)
			inPack = s == "packfile"
		default:
			switch p.Data[0] {
			case 1:
				pack = append(pack, p.Data[1:]...)
			case 2:
				progress = append(progress, p.Data[1:]...)
			default:
				t.Fatalf("side-band %d: %q", p.Data[0], p.Data[1:])
			}
		}
	}
}

// indexPack feeds pack into a fresh repository with git index-pack and
// returns that repository's path.
func indexPack(t testing.TB, pack []byte, fixThin bool, base string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dst.git")
	if base != "" {
		git(t, filepath.Dir(dir), "clone", "-q", "--bare", base, dir)
	} else {
		git(t, filepath.Dir(dir), "init", "-q", "--bare", dir)
	}
	args := []string{"index-pack", "--stdin", "--strict"}
	if fixThin {
		args = append(args, "--fix-thin")
	}
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Stdin = bytes.NewReader(pack)
	var errb bytes.Buffer
	c.Stderr = &errb
	if err := c.Run(); err != nil {
		t.Fatalf("index-pack: %v\n%s", err, errb.String())
	}
	return dir
}
