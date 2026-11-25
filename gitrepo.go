package gitproto

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// GitRepo is a Repository backed by the git binary on an on-disk repository.
//
// Milestone 1 delegates object reading and pack building to git: every
// command line is fixed in this file, nothing from the wire reaches argv
// (object ids are validated 40-hex strings written to stdin), no shell is
// involved, and git runs with a minimal environment and no user or system
// configuration.
type GitRepo struct {
	// Dir is the repository's git directory (a bare repository, or a
	// .git directory).
	Dir string
	// Git is the git executable. Empty means "git" found on PATH.
	Git string
	// PackThreads is passed to git pack-objects as --threads. Zero leaves
	// git's default (all cores).
	PackThreads int
}

func (g *GitRepo) cmd(ctx context.Context, args ...string) *exec.Cmd {
	bin := g.Git
	if bin == "" {
		bin = "git"
	}
	c := exec.CommandContext(ctx, bin, args...)
	env := []string{
		"GIT_DIR=" + g.Dir,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_NO_REPLACE_OBJECTS=1",
		"LC_ALL=C",
	}
	for _, k := range []string{"PATH", "SYSTEMROOT", "TMPDIR", "TEMP", "TMP"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	c.Env = env
	return c
}

// run executes a git command with stdin and returns stdout; a non-zero exit
// is an *exec.ExitError carrying a bounded stderr.
func (g *GitRepo) run(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	c := g.cmd(ctx, args...)
	var out bytes.Buffer
	errb := &boundedBuffer{max: 4096}
	c.Stdout, c.Stderr = &out, errb
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	if err := c.Run(); err != nil {
		return out.Bytes(), &gitError{args: args, err: err, stderr: errb.String()}
	}
	return out.Bytes(), nil
}

type gitError struct {
	args   []string
	err    error
	stderr string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("gitproto: git %s: %v: %s", e.args[0], e.err, strings.TrimSpace(e.stderr))
}
func (e *gitError) Unwrap() error { return e.err }

type boundedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
func (b *boundedBuffer) String() string { return b.buf.String() }

// cancelWriter forwards to w; when w fails, the consumer is gone: it records
// the error and cancels the context so git is killed instead of awaited.
type cancelWriter struct {
	w      io.Writer
	cancel context.CancelFunc
	err    error
}

func (c *cancelWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if err != nil {
		c.err = err
		c.cancel()
	}
	return n, err
}

// exitCode returns the exit status of a failed git command, or -1.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// Refs implements Repository.
func (g *GitRepo) Refs(ctx context.Context) ([]Ref, error) {
	out, err := g.run(ctx, "", "for-each-ref",
		"--format=%(objectname)%00%(refname)%00%(*objectname)%00%(symref)")
	if err != nil {
		return nil, err
	}
	var refs []Ref
	for line := range strings.SplitSeq(strings.TrimSuffix(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\x00")
		n := len(f)
		f = append(f, "", "", "", "") // indexing below never depends on the count check
		if n != 4 || !ValidOID(f[0]) {
			return nil, fmt.Errorf("gitproto: unexpected for-each-ref line %q", line)
		}
		r := Ref{ID: f[0], Name: f[1], Target: f[3]}
		if f[2] != "" {
			r.Peeled = f[2]
		}
		refs = append(refs, r)
	}
	// for-each-ref's %(*objectname) peels one level; peel fully.
	// One process for all of them: a process per tag costs more than the pack.
	var peel []int
	var in strings.Builder
	for i := range refs {
		if refs[i].Peeled != "" {
			peel = append(peel, i)
			in.WriteString(refs[i].ID + "^{}\n")
		}
	}
	if len(peel) > 0 {
		out, err := g.run(ctx, in.String(), "cat-file", "--batch-check=%(objectname)")
		if err != nil {
			return nil, err
		}
		lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		if len(lines) != len(peel) {
			return nil, fmt.Errorf("gitproto: cat-file answered %d lines for %d refs", len(lines), len(peel))
		}
		for k, i := range peel {
			switch {
			case ValidOID(lines[k]):
				refs[i].Peeled = lines[k]
			case strings.HasSuffix(lines[k], " missing"):
				// no longer resolves: keep for-each-ref's one-level answer
			default:
				return nil, fmt.Errorf("gitproto: unexpected cat-file line %q", lines[k])
			}
		}
	}

	head := Ref{Name: "HEAD"}
	if out, err := g.run(ctx, "", "symbolic-ref", "-q", "HEAD"); err == nil {
		head.Target = strings.TrimSpace(string(out))
	} else if exitCode(err) != 1 { // 1: HEAD is detached
		return nil, err
	}
	id, ok, err := g.revParse(ctx, "HEAD")
	if err != nil {
		return nil, err
	}
	switch {
	case ok:
		head.ID = id
		if p, ok, err := g.revParse(ctx, "HEAD^{}"); err != nil {
			return nil, err
		} else if ok && p != id {
			head.Peeled = p
		}
		refs = slices.Insert(refs, 0, head)
	case head.Target != "":
		refs = slices.Insert(refs, 0, head) // unborn
	}
	return refs, nil
}

// revParse resolves a fixed rev expression to an object id; ok is false when
// it does not resolve.
func (g *GitRepo) revParse(ctx context.Context, spec string) (id string, ok bool, err error) {
	out, err := g.run(ctx, "", "rev-parse", "-q", "--verify", spec)
	if err != nil {
		if exitCode(err) == 1 {
			return "", false, nil
		}
		return "", false, err
	}
	id = strings.TrimSpace(string(out))
	if !ValidOID(id) {
		return "", false, fmt.Errorf("gitproto: rev-parse returned %q", id)
	}
	return id, true, nil
}

// CommonCommits implements Repository.
func (g *GitRepo) CommonCommits(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var in strings.Builder
	for _, id := range ids {
		if !ValidOID(id) {
			return nil, fmt.Errorf("gitproto: invalid object id %q", id)
		}
		in.WriteString(id + "\n")
	}
	out, err := g.run(ctx, in.String(), "cat-file", "--batch-check=%(objectname) %(objecttype)")
	if err != nil {
		return nil, err
	}
	var common []string
	for line := range strings.SplitSeq(strings.TrimSuffix(string(out), "\n"), "\n") {
		if id, ok := strings.CutSuffix(line, " commit"); ok && ValidOID(id) {
			common = append(common, id)
		}
	}
	return common, nil
}

// WritePack implements Repository with git pack-objects --revs --stdout.
func (g *GitRepo) WritePack(ctx context.Context, req PackRequest, w io.Writer) error {
	args := []string{"pack-objects", "--revs", "--stdout", "--quiet"}
	if req.Thin {
		args = append(args, "--thin")
	}
	if req.OfsDelta {
		args = append(args, "--delta-base-offset")
	}
	if req.IncludeTag {
		args = append(args, "--include-tag")
	}
	if g.PackThreads > 0 {
		args = append(args, fmt.Sprintf("--threads=%d", g.PackThreads))
	}
	var in strings.Builder
	for _, id := range req.Wants {
		if !ValidOID(id) {
			return fmt.Errorf("gitproto: invalid object id %q", id)
		}
		in.WriteString(id + "\n")
	}
	for _, id := range req.Common {
		if !ValidOID(id) {
			return fmt.Errorf("gitproto: invalid object id %q", id)
		}
		in.WriteString("^" + id + "\n")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c := g.cmd(ctx, args...)
	c.Stdin = strings.NewReader(in.String())
	errb := &boundedBuffer{max: 4096}
	c.Stderr = errb
	cw := &cancelWriter{w: w, cancel: cancel}
	c.Stdout = cw
	if err := c.Start(); err != nil {
		return err
	}
	waitErr := c.Wait()
	if cw.err != nil {
		return cw.err
	}
	if waitErr != nil {
		return &gitError{args: args, err: waitErr, stderr: errb.String()}
	}
	return nil
}
