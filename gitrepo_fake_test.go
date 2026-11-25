package gitproto_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/go-vcs/gitproto"
)

// A scripted stand-in for the git binary: each sub-command (or, for rev-parse,
// each rev expression) has a canned stdout and exit status. It lets the tests
// hand GitRepo the answers real git never gives (malformed lines, odd exit
// codes), so every guard on git's output is held on both sides.

type reply struct {
	out string
	rc  int
}

const fakeGitScript = `#!/bin/sh
d=$(dirname "$0")
k=$1
if [ "$1" = rev-parse ]; then
  for a; do last=$a; done
  k=rev-parse.$(printf %s "$last" | tr -c 'A-Za-z0-9' _)
fi
cat >/dev/null
[ -f "$d/$k.out" ] && cat "$d/$k.out"
rc=0
[ -f "$d/$k.rc" ] && rc=$(cat "$d/$k.rc")
exit "$rc"
`

// fakeGit returns a GitRepo whose git answers per replies; a key without a
// reply prints nothing and exits 0.
func fakeGit(t *testing.T, replies map[string]reply) *gitproto.GitRepo {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the scripted git is a POSIX shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "git")
	write(t, bin, fakeGitScript)
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, r := range replies {
		write(t, filepath.Join(dir, k+".out"), r.out)
		write(t, filepath.Join(dir, k+".rc"), strconv.Itoa(r.rc))
	}
	return &gitproto.GitRepo{Dir: dir, Git: bin}
}

// revKey is the script's key for rev-parse of spec.
func revKey(spec string) string {
	var b strings.Builder
	for _, c := range []byte(spec) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	return "rev-parse." + b.String()
}

// refLine is a for-each-ref line whose peeled object, when present, is itself
// a tag (so Refs must ask cat-file for the full peel).
func refLine(id, name, peeled, symref string) string {
	typ := ""
	if peeled != "" {
		typ = "tag"
	}
	return refLineType(id, name, peeled, symref, typ)
}

func refLineType(id, name, peeled, symref, peeledType string) string {
	return id + "\x00" + name + "\x00" + peeled + "\x00" + symref + "\x00" + peeledType + "\n"
}

// A tag whose one-level peel is a commit is fully peeled: no cat-file runs
// (the scripted cat-file would fail the call if it did), and a HEAD that names
// a listed ref is answered from the listing without rev-parse.
func TestGitRepoRefsNeedsNoExtraProcessForTheCommonCase(t *testing.T) {
	r := fakeGit(t, map[string]reply{
		"for-each-ref": {refLine(idA, "refs/heads/main", "", "") + refLineType(idA, "refs/tags/t", idB, "", "commit"), 0},
		"cat-file":     {"", 128},
		"symbolic-ref": {"refs/heads/main\n", 0},
		revKey("HEAD"): {"", 128},
	})
	got, err := r.Refs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []gitproto.Ref{
		{Name: "HEAD", ID: idA, Target: "refs/heads/main"},
		{Name: "refs/heads/main", ID: idA},
		{Name: "refs/tags/t", ID: idA, Peeled: idB},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ref %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

// A HEAD whose target is not in the listing (unborn, or outside refs/) still
// goes through rev-parse.
func TestGitRepoRefsHeadOutsideTheListingUsesRevParse(t *testing.T) {
	r := fakeGit(t, map[string]reply{
		"for-each-ref":    {refLine(idA, "refs/heads/main", "", ""), 0},
		"symbolic-ref":    {"refs/heads/other\n", 0},
		revKey("HEAD"):    {idB + "\n", 0},
		revKey("HEAD^{}"): {idB + "\n", 0},
	})
	got, err := r.Refs(context.Background())
	if err != nil || len(got) != 2 || got[0].ID != idB || got[0].Target != "refs/heads/other" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestGitRepoRefsParsesGitOutput(t *testing.T) {
	idC := strings.Repeat("c", 40)
	r := fakeGit(t, map[string]reply{
		"for-each-ref":    {refLine(idA, "refs/heads/main", "", "") + refLine(idA, "refs/tags/t", idB, ""), 0},
		"cat-file":        {idC + "\n", 0},
		"symbolic-ref":    {"refs/heads/main\n", 0},
		revKey("HEAD"):    {idA + "\n", 0},
		revKey("HEAD^{}"): {idB + "\n", 0},
	})
	got, err := r.Refs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []gitproto.Ref{
		{Name: "HEAD", ID: idA, Target: "refs/heads/main"},
		{Name: "refs/heads/main", ID: idA},
		{Name: "refs/tags/t", ID: idA, Peeled: idC},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ref %d: got %+v want %+v", i, got[i], want[i])
		}
	}

	// A peeled name that no longer resolves keeps for-each-ref's one-level
	// answer; a detached HEAD has no target; an unresolvable HEAD is absent.
	r = fakeGit(t, map[string]reply{
		"for-each-ref": {refLine(idA, "refs/tags/t", idB, ""), 0},
		"cat-file":     {idA + "^{} missing\n", 0},
		"symbolic-ref": {"", 1},
		revKey("HEAD"): {"", 1},
	})
	got, err = r.Refs(context.Background())
	if err != nil || len(got) != 1 || got[0].Peeled != idB {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestGitRepoRefsRefusesWhatGitGetsWrong(t *testing.T) {
	okRefs := refLine(idA, "refs/heads/main", "", "")
	base := func() map[string]reply {
		return map[string]reply{
			"for-each-ref": {okRefs, 0},
			"symbolic-ref": {"", 1},
			revKey("HEAD"): {"", 1},
		}
	}
	// Sanity: the base answers are accepted.
	if _, err := fakeGit(t, base()).Refs(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(map[string]reply){
		"for-each-ref fails":  func(m map[string]reply) { m["for-each-ref"] = reply{okRefs, 2} },
		"line with 3 fields":  func(m map[string]reply) { m["for-each-ref"] = reply{idA + "\x00refs/x\x00\n", 0} },
		"line with 6 fields":  func(m map[string]reply) { m["for-each-ref"] = reply{okRefs[:len(okRefs)-1] + "\x00z\n", 0} },
		"invalid object name": func(m map[string]reply) { m["for-each-ref"] = reply{refLine("zz", "refs/x", "", ""), 0} },
		"peel fails": func(m map[string]reply) {
			m["for-each-ref"] = reply{refLine(idA, "refs/t", idB, ""), 0}
			m["cat-file"] = reply{idB + "\n", 128} // plausible output, failed exit
		},
		"peel answers too few lines": func(m map[string]reply) {
			m["for-each-ref"] = reply{refLine(idA, "refs/t", idB, ""), 0}
			m["cat-file"] = reply{"", 0}
		},
		"peel answers too many lines": func(m map[string]reply) {
			m["for-each-ref"] = reply{refLine(idA, "refs/t", idB, ""), 0}
			m["cat-file"] = reply{idB + "\n" + idB + "\n", 0}
		},
		"peel answers garbage": func(m map[string]reply) {
			m["for-each-ref"] = reply{refLine(idA, "refs/t", idB, ""), 0}
			m["cat-file"] = reply{"garbage\n", 0}
		},
		"symbolic-ref fails":   func(m map[string]reply) { m["symbolic-ref"] = reply{"", 128} },
		"rev-parse HEAD fails": func(m map[string]reply) { m[revKey("HEAD")] = reply{"", 128} },
		"rev-parse HEAD^{} fails": func(m map[string]reply) {
			m[revKey("HEAD")] = reply{idA + "\n", 0}
			m[revKey("HEAD^{}")] = reply{"", 128}
		},
		"rev-parse prints garbage": func(m map[string]reply) { m[revKey("HEAD")] = reply{"not-an-oid\n", 0} },
		"rev-parse prints nothing": func(m map[string]reply) { m[revKey("HEAD")] = reply{"", 0} },
	} {
		m := base()
		mod(m)
		if _, err := fakeGit(t, m).Refs(context.Background()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestGitRepoNeverRunsGitForNothing(t *testing.T) {
	missing := &gitproto.GitRepo{Dir: t.TempDir(), Git: filepath.Join(t.TempDir(), "no-such-git")}
	got, err := missing.CommonCommits(context.Background(), nil)
	if err != nil || got != nil {
		t.Fatalf("CommonCommits(nil) ran git: %v %v", got, err)
	}
	// And a missing binary is the start error itself, not a git failure.
	err = missing.WritePack(context.Background(), gitproto.PackRequest{Wants: []string{idA}}, &bytes.Buffer{})
	if err == nil || strings.Contains(err.Error(), "pack-objects") {
		t.Fatalf("missing git binary: %v", err)
	}
}

func TestGitRepoWritePackValidatesBeforeAndOnlyBefore(t *testing.T) {
	r := fakeGit(t, map[string]reply{"pack-objects": {"PACK", 0}})
	ctx := context.Background()
	var b bytes.Buffer
	if err := r.WritePack(ctx, gitproto.PackRequest{Wants: []string{idA}, Common: []string{idB}}, &b); err != nil || b.String() != "PACK" {
		t.Fatalf("valid request: %v %q", err, b.String())
	}
	for name, req := range map[string]gitproto.PackRequest{
		"want":   {Wants: []string{"--all"}},
		"common": {Wants: []string{idA}, Common: []string{"--all"}},
	} {
		b.Reset()
		if err := r.WritePack(ctx, req, &b); err == nil {
			t.Errorf("%s: invalid id accepted", name)
		}
		if b.Len() != 0 {
			t.Errorf("%s: git ran on an invalid id", name)
		}
	}
}

func TestGitRepoCommonCommitsKeepsOnlyCommits(t *testing.T) {
	r := fakeGit(t, map[string]reply{
		"cat-file": {idA + " commit\n" + idB + " tree\n" + "missing\n", 0},
	})
	got, err := r.CommonCommits(context.Background(), []string{idA, idB})
	if err != nil || len(got) != 1 || got[0] != idA {
		t.Fatalf("%v %v", got, err)
	}
}
