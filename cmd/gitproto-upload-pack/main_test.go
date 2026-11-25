package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/go-vcs/gitproto/pktline"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.org",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.org")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func makeRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "init", "-q", "-b", "main")
	for _, s := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(work, "f"), []byte(strings.Repeat(s, 5000)), 0o644); err != nil {
			t.Fatal(err)
		}
		gitRun(t, work, "add", "f")
		gitRun(t, work, "commit", "-q", "-m", s)
	}
	gitRun(t, work, "tag", "-a", "-m", "v1", "v1")
	bare := filepath.Join(root, "bare.git")
	gitRun(t, root, "clone", "-q", "--bare", work, bare)
	return bare
}

func run(t *testing.T, env string, in string, args ...string) (out, errOut string, err error) {
	t.Helper()
	t.Setenv("GIT_PROTOCOL", env)
	cmd := RootCmd()
	var o, e bytes.Buffer
	cmd.SetIn(strings.NewReader(in))
	cmd.SetOut(&o)
	cmd.SetErr(&e)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return o.String(), e.String(), err
}

func TestRefusesWithoutProtocolV2(t *testing.T) {
	repo := makeRepo(t)
	for _, env := range []string{"", "version=1", "version=0", "foo=bar:version=3", "version=22"} {
		out, errOut, err := run(t, env, "", repo)
		if err == nil || out != "" || !strings.Contains(errOut, "version=2") {
			t.Errorf("GIT_PROTOCOL=%q: err=%v out=%q err=%q", env, err, out, errOut)
		}
	}
	if _, _, err := run(t, "foo=bar:version=2", "0000", repo); err != nil {
		t.Errorf("version=2 among other keys refused: %v", err)
	}
}

func TestRefusesNonDirectory(t *testing.T) {
	_, errOut, err := run(t, "version=2", "", filepath.Join(t.TempDir(), "nope"))
	if err == nil || !strings.Contains(errOut, "not a directory") {
		t.Fatalf("%v %q", err, errOut)
	}
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, "version=2", "", file); err == nil {
		t.Fatal("a file was accepted as a repository")
	}
}

func TestArgsAreRequired(t *testing.T) {
	if _, _, err := run(t, "version=2", ""); err == nil {
		t.Fatal("no directory accepted")
	}
	if _, _, err := run(t, "version=2", "", "a", "b"); err == nil {
		t.Fatal("two directories accepted")
	}
}

func TestAdvertiseRefs(t *testing.T) {
	out, _, err := run(t, "version=2", "", "--advertise-refs", makeRepo(t))
	if err != nil || !strings.HasPrefix(out, "000eversion 2\n") || !strings.HasSuffix(out, "0000") {
		t.Fatalf("%v %q", err, out)
	}
}

func TestStatelessRPC(t *testing.T) {
	repo := makeRepo(t)
	req := "0014command=ls-refs\n00010009peel\n0000"
	out, _, err := run(t, "version=2", req, "--stateless-rpc", repo)
	if err != nil || !strings.Contains(out, "refs/heads/main") || strings.Contains(out, "version 2") {
		t.Fatalf("%v %q", err, out)
	}
	// An empty request is not an error.
	if _, _, err := run(t, "version=2", "0000", "--stateless-rpc", repo); err != nil {
		t.Fatal(err)
	}
	// A refused request is.
	out, errOut, err := run(t, "version=2", "0011command=push\n0000", "--stateless-rpc", repo)
	if err == nil || !strings.Contains(out, "ERR unsupported command") || !strings.Contains(errOut, "unsupported command") {
		t.Fatalf("%v %q %q", err, out, errOut)
	}
}

func TestSessionOverStdio(t *testing.T) {
	repo := makeRepo(t)
	in := "0014command=ls-refs\n00010009peel\n0000" + "0000"
	out, _, err := run(t, "version=2", in, repo)
	if err != nil {
		t.Fatal(err)
	}
	r := pktline.NewReader(strings.NewReader(out))
	p, _ := r.Read()
	if string(p.Data) != "version 2\n" {
		t.Fatalf("%q", out)
	}
}

func TestProtocolV2(t *testing.T) {
	for in, want := range map[string]bool{
		"version=2": true, "a=b:version=2": true, "version=2:a=b": true,
		"": false, "version=1": false, "version=2x": false, "xversion=2": false,
	} {
		if protocolV2(in) != want {
			t.Errorf("protocolV2(%q) != %v", in, want)
		}
	}
}

func TestGitDirPrefersDotGit(t *testing.T) {
	dir := t.TempDir()
	if gitDir(dir) != dir {
		t.Fatal("bare dir changed")
	}
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if gitDir(dir) != filepath.Join(dir, ".git") {
		t.Fatal(".git not preferred")
	}
}

func TestMainExitsNonZeroOnRefusal(t *testing.T) {
	t.Setenv("GIT_PROTOCOL", "")
	old := osExit
	defer func() { osExit = old }()
	code := -1
	osExit = func(c int) { code = c }
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"gitproto-upload-pack", t.TempDir()}
	main()
	if code != 1 {
		t.Fatalf("exit code %d", code)
	}
}

// TestGitCloneAndFetchThroughTheBinary drives the real git client: protocol
// v2, --upload-pack pointing at the built binary, over file://.
func TestGitCloneAndFetchThroughTheBinary(t *testing.T) {
	name := "gitproto-upload-pack"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// git runs --upload-pack through its own sh, which would eat the
	// backslashes of a Windows path: hand it forward slashes.
	bin = filepath.ToSlash(bin)
	bare := makeRepo(t)
	root := t.TempDir()
	url := "file://" + filepath.ToSlash(bare)
	clone := filepath.Join(root, "clone")
	gitRun(t, root, "-c", "protocol.version=2", "clone", "-q", "--upload-pack="+bin, url, clone)
	gitRun(t, clone, "fsck", "--strict")
	if gitRun(t, clone, "rev-parse", "origin/main") != gitRun(t, bare, "rev-parse", "main") {
		t.Fatal("main differs")
	}
	if gitRun(t, clone, "rev-parse", "v1") != gitRun(t, bare, "rev-parse", "v1") {
		t.Fatal("tag differs")
	}

	// New commit on the server, then an incremental fetch (haves, thin pack).
	work := filepath.Join(filepath.Dir(bare), "work")
	if err := os.WriteFile(filepath.Join(work, "f"), []byte(strings.Repeat("a", 5000)+"x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "commit", "-q", "-a", "-m", "d")
	gitRun(t, work, "push", "-q", bare, "main")
	gitRun(t, clone, "-c", "protocol.version=2", "fetch", "-q", "--upload-pack="+bin)
	if gitRun(t, clone, "rev-parse", "origin/main") != gitRun(t, bare, "rev-parse", "main") {
		t.Fatal("main differs after fetch")
	}
	gitRun(t, clone, "fsck", "--strict")

	// Unsupported capabilities are refused: the client cannot go shallow.
	for _, extra := range [][]string{{"--depth", "1"}, {"--filter=blob:none"}} {
		args := append([]string{"-c", "protocol.version=2", "clone", "-q", "--upload-pack=" + bin}, extra...)
		args = append(args, url, filepath.Join(root, "refused"+extra[0][2:4]))
		c := exec.Command("git", args...)
		c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull)
		out, err := c.CombinedOutput()
		if extra[0] == "--depth" && err == nil {
			t.Errorf("shallow clone succeeded:\n%s", out)
		}
		_ = out
	}

	// Protocol v0 and v1 are refused.
	for _, v := range []string{"0", "1"} {
		c := exec.Command("git", "-c", "protocol.version="+v, "clone", "-q", "--upload-pack="+bin, url, filepath.Join(root, "v"+v))
		c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull)
		if out, err := c.CombinedOutput(); err == nil {
			t.Errorf("protocol.version=%s clone succeeded:\n%s", v, out)
		}
	}
}
