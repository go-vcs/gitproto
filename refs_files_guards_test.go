package gitproto

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// layout writes a git directory (config "[core]", HEAD on refs/heads/main)
// with the given files, relative to it, and returns a repo whose git is dead.
func layout(t *testing.T, files map[string]string) *GitRepo {
	t.Helper()
	dir := t.TempDir()
	all := map[string]string{"config": "[core]\n", "HEAD": "ref: refs/heads/main\n"}
	for k, v := range files {
		all[k] = v
	}
	if err := os.MkdirAll(filepath.Join(dir, "refs", "heads"), 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range all {
		p := filepath.Join(dir, k)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &GitRepo{Dir: dir, Git: filepath.Join(dir, "no-such-git")}
}

// native runs refsNative with a deadline, so that a loop that stops
// terminating fails the test instead of hanging it.
func native(t *testing.T, g *GitRepo) ([]Ref, error) {
	t.Helper()
	type res struct {
		refs []Ref
		err  error
	}
	c := make(chan res, 1)
	go func() {
		r, err := g.refsNative(context.Background())
		c <- res{r, err}
	}()
	select {
	case r := <-c:
		return r.refs, r.err
	case <-time.After(10 * time.Second):
		t.Fatal("refsNative did not terminate")
		return nil, nil
	}
}

func TestNativeRefsRefusals(t *testing.T) {
	hdr := "# pack-refs with: peeled fully-peeled sorted \n"
	cases := map[string]map[string]string{
		"no HEAD":                 {"HEAD": ""},
		"garbage loose ref":       {"refs/heads/main": "hello\n"},
		"loose ref to HEAD":       {"refs/heads/main": "ref: HEAD\n"},
		"loose ref to bad name":   {"refs/heads/main": "ref: refs/a..b\n"},
		"loose bad name":          {"refs/heads/x~y": oidA + "\n"},
		"HEAD to non-refs":        {"HEAD": "ref: HEAD\n"},
		"packed bad header":       {"packed-refs": "# something else\n"},
		"packed fully-peeled hdr": {"packed-refs": "# fully-peeled\n" + oidA + " refs/heads/main\n"},
		"dangling symref":         {"refs/heads/a": "ref: refs/heads/nope\n"},
		"symref cycle":            {"refs/heads/a": "ref: refs/heads/b\n", "refs/heads/b": "ref: refs/heads/a\n"},
		"self symref":             {"refs/heads/a": "ref: refs/heads/a\n"},
		"config section unclosed": {"config": "[extensions\n\trefStorage = files\n"},
		"config lookalike key":    {"config": "[extensions]\n\trefStorageFoo = files\n"},
		"config bare key":         {"config": "[extensions]\n\trefStorage\n"},
	}
	_ = hdr
	for name, files := range cases {
		_, err := native(t, layout(t, files))
		if !errors.Is(err, errUnsupported) {
			t.Errorf("%s: got %v, want errUnsupported", name, err)
		}
	}
	// No HEAD at all.
	nh := layout(t, nil)
	if err := os.Remove(filepath.Join(nh.Dir, "HEAD")); err != nil {
		t.Fatal(err)
	}
	if _, err := native(t, nh); !errors.Is(err, errUnsupported) {
		t.Errorf("missing HEAD: %v", err)
	}
	// No refs/ at all.
	g := layout(t, nil)
	if err := os.RemoveAll(filepath.Join(g.Dir, "refs")); err != nil {
		t.Fatal(err)
	}
	if _, err := native(t, g); !errors.Is(err, errUnsupported) {
		t.Errorf("no refs/: %v", err)
	}
	// A loose ref that is a symlink.
	if runtime.GOOS != "windows" {
		g := layout(t, nil)
		if err := os.Symlink("main", filepath.Join(g.Dir, "refs", "heads", "l")); err != nil {
			t.Fatal(err)
		}
		if _, err := native(t, g); !errors.Is(err, errUnsupported) {
			t.Errorf("symlink: %v", err)
		}
	}
}

// A chain of five links resolves, six do not: the bound is git's, and it is
// asserted on both sides.
func TestSymrefChainBound(t *testing.T) {
	chain := func(n int) *GitRepo {
		files := map[string]string{"refs/heads/end": oidA + "\n"}
		for i := 0; i < n; i++ {
			next := "refs/heads/s" + string(rune('0'+i+1))
			if i == n-1 {
				next = "refs/heads/end"
			}
			files["refs/heads/s"+string(rune('0'+i))] = "ref: " + next + "\n"
		}
		return layout(t, files)
	}
	got, err := native(t, chain(maxSymrefDepth))
	if err != nil {
		t.Fatalf("chain of %d: %v", maxSymrefDepth, err)
	}
	var s0 Ref
	for _, r := range got {
		if r.Name == "refs/heads/s0" {
			s0 = r
		}
	}
	if s0.ID != oidA || s0.Target != "refs/heads/end" {
		t.Fatalf("s0 = %+v", s0)
	}
	if _, err := native(t, chain(maxSymrefDepth+1)); !errors.Is(err, errUnsupported) {
		t.Fatalf("chain of %d: %v", maxSymrefDepth+1, err)
	}
}

func TestNativeRefsCountBounds(t *testing.T) {
	old := maxRefCount
	defer func() { maxRefCount = old }()
	hdr := "# pack-refs with: peeled fully-peeled sorted \n"
	var eb *errRefBound

	// Loose walk: two refs, bound of one.
	two := map[string]string{"refs/heads/a": oidA + "\n", "refs/heads/b": oidA + "\n"}
	maxRefCount = 1
	if _, err := native(t, layout(t, two)); !errors.As(err, &eb) {
		t.Errorf("loose count: %v", err)
	}
	maxRefCount = 2
	if _, err := native(t, layout(t, two)); err != nil {
		t.Errorf("loose count at the bound: %v", err)
	}

	// Merge: one loose, two packed, bound of two.
	mixed := map[string]string{
		"refs/heads/a": oidA + "\n",
		"packed-refs":  hdr + oidA + " refs/heads/p\n" + oidA + " refs/heads/q\n",
	}
	maxRefCount = 2
	if _, err := native(t, layout(t, mixed)); !errors.As(err, &eb) {
		t.Errorf("merge count: %v", err)
	}
	maxRefCount = 3
	if _, err := native(t, layout(t, mixed)); err != nil {
		t.Errorf("merge count at the bound: %v", err)
	}
}

func TestHeadBound(t *testing.T) {
	g := layout(t, map[string]string{"HEAD": "ref: refs/heads/" + strings.Repeat("x", maxRefLineBytes) + "\n"})
	var eb *errRefBound
	if _, err := native(t, g); !errors.As(err, &eb) {
		t.Fatalf("oversized HEAD: %v", err)
	}
}

func TestWalkLooseVanishedDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	all := map[string]*rawRef{}
	// Below refs/ a directory packed away during the walk is not an error ...
	if err := walkLoose(context.Background(), missing, "refs/x", 1, all); err != nil {
		t.Fatalf("depth 1: %v", err)
	}
	// ... but refs/ itself missing is not a repository we read.
	if err := walkLoose(context.Background(), missing, "refs", 0, all); !errors.Is(err, errUnsupported) {
		t.Fatalf("depth 0: %v", err)
	}
	// A path that is not a directory is not a vanished one.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// (Windows reports a file read as a directory as "path not found".)
	if err := walkLoose(context.Background(), f, "refs/x", 1, all); runtime.GOOS != "windows" && !errors.Is(err, errUnsupported) {
		t.Fatalf("not a dir: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("%v", all)
	}
}

func TestReadBounded(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("abcd"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := readBounded(file, 4); err != nil || string(b) != "abcd" {
		t.Fatalf("at the bound: %q %v", b, err)
	}
	var eb *errRefBound
	if _, err := readBounded(file, 3); !errors.As(err, &eb) {
		t.Fatalf("over the bound: %v", err)
	}
	// An oversized file is refused from its size, without reading it.
	oldRead := osReadFile
	osReadFile = func(string) ([]byte, error) { t.Error("read an oversized file"); return nil, nil }
	_, _ = readBounded(file, 3)
	osReadFile = oldRead
	if _, err := readBounded(filepath.Join(dir, "none"), 4); !errors.Is(err, fs.ErrNotExist) || errors.Is(err, errUnsupported) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := readBounded(dir, 4); !errors.Is(err, errUnsupported) {
		t.Fatalf("a directory: %v", err)
	}
	if runtime.GOOS != "windows" {
		ln := filepath.Join(dir, "ln")
		if err := os.Symlink(file, ln); err != nil {
			t.Fatal(err)
		}
		if _, err := readBounded(ln, 4); !errors.Is(err, errUnsupported) {
			t.Fatalf("a symlink: %v", err)
		}
	}

	old := osReadFile
	defer func() { osReadFile = old }()
	osReadFile = func(string) ([]byte, error) { return []byte("abcdef"), nil }
	if _, err := readBounded(file, 4); !errors.As(err, &eb) { // the file grew after the stat
		t.Fatalf("grown file: %v", err)
	}
	boom := errors.New("boom")
	osReadFile = func(string) ([]byte, error) { return nil, boom }
	if _, err := readBounded(file, 4); !errors.Is(err, boom) {
		t.Fatalf("read error: %v", err)
	}
	// readRefFile turns any other error into "not read natively", a missing
	// file stays missing.
	if _, err := readRefFile(file); !errors.Is(err, errUnsupported) {
		t.Fatalf("readRefFile: %v", err)
	}
	if _, err := readRefFile(filepath.Join(dir, "none")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("readRefFile missing: %v", err)
	}
}

func TestLooseFileVanishedIsSkipped(t *testing.T) {
	// A file listed and then gone (packed away) is skipped, not fatal.
	g := layout(t, map[string]string{"refs/heads/a": oidA + "\n"})
	old := osReadFile
	defer func() { osReadFile = old }()
	osReadFile = func(p string) ([]byte, error) {
		if filepath.Base(p) == "a" {
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
		}
		return old(p)
	}
	got, err := native(t, g)
	if err != nil || len(got) != 1 || got[0].Name != "HEAD" {
		t.Fatalf("%+v %v", got, err)
	}
}

// fakeGit writes an executable that answers cat-file --batch-check with
// body and exits with code.
func fakeGit(t *testing.T, body string, code string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script stand-in for git")
	}
	p := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '" + body + "'\nexit " + code + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPeelAnswers(t *testing.T) {
	// A loose tag outside refs/heads/ is peeled by one cat-file call.
	files := map[string]string{"refs/tags/t": oidA + "\n", "refs/heads/main": oidA + "\n"}
	run := func(body, code string) ([]Ref, error) {
		g := layout(t, files)
		g.Git = fakeGit(t, body, code)
		return native(t, g)
	}
	peeled := func(refs []Ref, name string) string {
		for _, r := range refs {
			if r.Name == name {
				return r.Peeled
			}
		}
		return "?"
	}
	got, err := run(oidB+"\\n", "0")
	if err != nil || peeled(got, "refs/tags/t") != oidB || peeled(got, "refs/heads/main") != "" {
		t.Fatalf("peeled: %+v %v", got, err)
	}
	got, err = run(oidA+"\\n", "0") // a commit: its peel is itself, recorded as none
	if err != nil || peeled(got, "refs/tags/t") != "" {
		t.Fatalf("commit: %+v %v", got, err)
	}
	if _, err := run("missing\\n", "0"); !errors.Is(err, errUnsupported) {
		t.Fatalf("missing object: %v", err)
	}
	if _, err := run("\\n", "0"); !errors.Is(err, errUnsupported) {
		t.Fatalf("empty answer: %v", err)
	}
	if _, err := run(oidB+"\\n"+oidB+"\\n", "0"); err == nil || errors.Is(err, errUnsupported) {
		t.Fatalf("two answers for one ref: %v", err)
	}
	got, err = run(oidB+"\\n", "1")
	if err == nil || got != nil || errors.Is(err, errUnsupported) {
		t.Fatalf("git failing: %+v %v", got, err)
	}
}
