package gitproto_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/go-vcs/gitproto"
)

// deadGit is a git executable that does not exist: a Refs call that succeeds
// with it read the refs without a process.
func deadGit(dir string) string { return filepath.Join(dir, "no-such-git") }

func bareClone(t *testing.T, f *fixture, name string) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), name)
	git(t, filepath.Dir(d), "clone", "-q", "--bare", f.Work, d)
	return d
}

// TestNativeRefsMatchGit compares the direct reader with git for-each-ref on
// the ref layouts a repository passes through, and where it can, proves that
// no process was spawned.
func TestNativeRefsMatchGit(t *testing.T) {
	f := newFixture(t)

	packedOnly := bareClone(t, f, "packed.git")
	git(t, packedOnly, "pack-refs", "--all")
	if _, err := os.Stat(filepath.Join(packedOnly, "packed-refs")); err != nil {
		t.Fatal(err)
	}

	empty := filepath.Join(t.TempDir(), "empty.git")
	git(t, filepath.Dir(empty), "init", "-q", "--bare", "-b", "main", empty)

	loosePushed := f.Bare // refs pushed after the clone are loose
	looseOverride := bareClone(t, f, "override.git")
	git(t, looseOverride, "update-ref", "refs/heads/topic", f.Main) // loose overrides packed
	git(t, looseOverride, "update-ref", "refs/tags/light", f.Topic)
	git(t, looseOverride, "tag", "-f", "-a", "-m", "again", "v1", f.Topic) // a loose annotated tag over a packed one
	headAtTag := bareClone(t, f, "headtag.git")
	git(t, headAtTag, "symbolic-ref", "HEAD", "refs/tags/v1v1") // HEAD naming a tag ref (packed)
	withSym := bareClone(t, f, "sym.git")
	git(t, withSym, "symbolic-ref", "refs/heads/alias", "refs/heads/topic")
	git(t, withSym, "symbolic-ref", "refs/heads/alias2", "refs/heads/alias") // a chain
	dangling := bareClone(t, f, "dangling.git")
	git(t, dangling, "symbolic-ref", "refs/heads/dangler", "refs/heads/nowhere")
	unborn := bareClone(t, f, "unborn.git")
	git(t, unborn, "symbolic-ref", "HEAD", "refs/heads/nowhere")
	detached := bareClone(t, f, "detached.git")
	write(t, filepath.Join(detached, "HEAD"), f.Topic+"\n")

	// needsNoGit: layouts the reader must answer by itself.
	for name, c := range map[string]struct {
		dir        string
		needsNoGit bool
	}{
		"packed only":     {packedOnly, true},
		"empty":           {empty, true},
		"loose pushed":    {loosePushed, true}, // loose refs/heads/ only
		"loose override":  {looseOverride, false},
		"head at tag":     {headAtTag, true},
		"symref chain":    {withSym, true},
		"dangling symref": {dangling, false}, // git decides
		"unborn":          {unborn, true},
		"detached":        {detached, false}, // git decides
	} {
		want, err := (&gitproto.GitRepo{Dir: c.dir, NoNativeRefs: true}).Refs(context.Background())
		if err != nil {
			t.Fatalf("%s: git: %v", name, err)
		}
		g := &gitproto.GitRepo{Dir: c.dir}
		if c.needsNoGit {
			g.Git = deadGit(t.TempDir())
		}
		got, err := g.Refs(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", name, got, want)
		}
	}
}

// A reftable repository, or an unknown extensions.refStorage, is never read
// as files: the stub refs/ would answer "no refs" with no error.
func TestNativeRefsNeverMisreadOtherBackends(t *testing.T) {
	f := newFixture(t)
	rt := filepath.Join(t.TempDir(), "rt")
	if !tryGit(filepath.Dir(rt), "init", "-q", "--bare", "--ref-format=reftable", "-b", "main", rt) {
		t.Skip("this git cannot create a reftable repository")
	}
	git(t, rt, "fetch", "-q", f.Bare, "main:refs/heads/main")
	git(t, rt, "tag", "-a", "-m", "t", "rt1", "refs/heads/main")
	gitDir := rt

	want, err := (&gitproto.GitRepo{Dir: gitDir, NoNativeRefs: true}).Refs(context.Background())
	if err != nil || len(want) < 3 {
		t.Fatalf("git on the reftable repository: %+v %v", want, err)
	}
	got, err := (&gitproto.GitRepo{Dir: gitDir}).Refs(context.Background())
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("reftable repository:\n got %+v (%v)\nwant %+v", got, err, want)
	}
	// With no git to fall back on, the call must fail, not answer.
	if got, err := (&gitproto.GitRepo{Dir: gitDir, Git: deadGit(t.TempDir())}).Refs(context.Background()); err == nil {
		t.Fatalf("a reftable repository was read without git: %+v", got)
	}

	// A files repository whose config names a backend this reader does not
	// know: same refusal to guess.
	future := bareClone(t, f, "future.git")
	cfg := filepath.Join(future, "config")
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	write(t, cfg, string(b)+"[extensions]\n\trefStorage = from-the-future\n")
	if got, err := (&gitproto.GitRepo{Dir: future, Git: deadGit(t.TempDir())}).Refs(context.Background()); err == nil {
		t.Fatalf("an unknown refStorage was read as files: %+v", got)
	}
	// And a worktree-style git directory (commondir) is not read either.
	wt := bareClone(t, f, "wt.git")
	write(t, filepath.Join(wt, "commondir"), "../elsewhere\n")
	if got, err := (&gitproto.GitRepo{Dir: wt, Git: deadGit(t.TempDir())}).Refs(context.Background()); err == nil {
		t.Fatalf("a commondir repository was read natively: %+v", got)
	}
}

// tryGit reports whether git ran successfully.
func tryGit(dir string, args ...string) bool {
	c := exec.Command("git", args...)
	c.Dir = dir
	return c.Run() == nil
}
