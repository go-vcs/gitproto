package gitproto_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-vcs/gitproto"
)

// TestPhases times, interleaved and in rotating order, the pieces of a clone
// fetch: Refs, WritePack, ServeRequest, pack-objects spawned directly with our
// arguments, and git upload-pack itself. Set GITPROTO_BENCH_REPO.
func TestPhases(t *testing.T) {
	dir := os.Getenv("GITPROTO_BENCH_REPO")
	if dir == "" {
		t.Skip("GITPROTO_BENCH_REPO unset")
	}
	gitDir := strings.TrimSpace(benchGitT(t, dir, "rev-parse", "--absolute-git-dir"))
	tip := strings.TrimSpace(benchGitT(t, dir, "rev-parse", "HEAD"))
	g := &gitproto.GitRepo{Dir: gitDir, PackThreads: 1}
	srv := &gitproto.Server{Repo: g}
	ctx := context.Background()
	req := fetchReq("want "+tip, "ofs-delta", "done")
	lean := []string{"GIT_DIR=" + gitDir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "PATH=" + os.Getenv("PATH")}

	steps := []struct {
		name string
		run  func() error
	}{
		{"Refs", func() error { _, err := g.Refs(ctx); return err }},
		{"WritePack", func() error {
			var o countingDiscard
			return g.WritePack(ctx, gitproto.PackRequest{Wants: []string{tip}, OfsDelta: true}, &o)
		}},
		{"ServeRequest", func() error {
			var o countingDiscard
			return srv.ServeRequest(ctx, bytes.NewReader(req), &o)
		}},
		{"direct pack-objects, our args", func() error {
			var o countingDiscard
			c := exec.Command("git", "pack-objects", "--revs", "--stdout", "--quiet", "--delta-base-offset", "--threads=1")
			c.Env, c.Stdin, c.Stdout = lean, strings.NewReader(tip+"\n"), &o
			return c.Run()
		}},
		{"git upload-pack", func() error {
			var o countingDiscard
			c := exec.Command("git", "-c", "pack.threads=1", "upload-pack", "--stateless-rpc", gitDir)
			c.Env = append(os.Environ(), "GIT_PROTOCOL=version=2")
			c.Stdin, c.Stdout = bytes.NewReader(req), &o
			return c.Run()
		}},
	}
	const n = 20
	times := make([][]time.Duration, len(steps))
	for i := range n {
		for k := range steps {
			s := (k + i) % len(steps) // rotate the starting step
			t0 := time.Now()
			if err := steps[s].run(); err != nil {
				t.Fatal(steps[s].name, err)
			}
			times[s] = append(times[s], time.Since(t0))
		}
	}
	for k, s := range steps {
		d := slices.Clone(times[k])
		slices.Sort(d)
		t.Logf("%-32s median %7.2f ms   min %7.2f ms", s.name, float64(d[n/2].Microseconds())/1000, float64(d[0].Microseconds())/1000)
	}
}

func benchGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
