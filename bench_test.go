package gitproto_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/go-vcs/gitproto"
)

// BenchmarkFetchClone compares a full clone fetch served by this package
// with the one served by `git upload-pack --stateless-rpc`, both on one core
// (pack.threads=1 / PackThreads: 1), on the repository named by
// GITPROTO_BENCH_REPO. The two sides alternate within every iteration so that
// load drifts hit both equally; each side's time is reported as its own
// metric (gitproto-ns/op and git-ns/op). See BENCHMARK.md.
func BenchmarkFetchClone(b *testing.B) {
	dir := os.Getenv("GITPROTO_BENCH_REPO")
	if dir == "" {
		b.Skip("set GITPROTO_BENCH_REPO to a repository to benchmark against")
	}
	gitDir := strings.TrimSpace(benchGit(b, dir, "rev-parse", "--absolute-git-dir"))
	tip := strings.TrimSpace(benchGit(b, dir, "rev-parse", "HEAD"))
	req := fetchReq("want "+tip, "ofs-delta", "done")

	srv := &gitproto.Server{Repo: &gitproto.GitRepo{Dir: gitDir, PackThreads: 1}}
	var ours, theirs time.Duration
	var oursBytes, theirsBytes int
	b.ResetTimer()
	b.StopTimer()
	for i := 0; i < b.N; i++ {
		var o countingDiscard
		t0 := time.Now()
		b.StartTimer()
		if err := srv.ServeRequest(context.Background(), bytes.NewReader(req), &o); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		ours += time.Since(t0)
		oursBytes = o.n

		var g countingDiscard
		c := exec.Command("git", "-c", "pack.threads=1", "upload-pack", "--stateless-rpc", gitDir)
		c.Env = append(os.Environ(), "GIT_PROTOCOL=version=2")
		c.Stdin = bytes.NewReader(req)
		c.Stdout = &g
		t1 := time.Now()
		if err := c.Run(); err != nil {
			b.Fatal(err)
		}
		theirs += time.Since(t1)
		theirsBytes = g.n
	}
	b.ReportMetric(float64(ours.Nanoseconds())/float64(b.N), "gitproto-ns/op")
	b.ReportMetric(float64(theirs.Nanoseconds())/float64(b.N), "git-ns/op")
	b.ReportMetric(float64(ours)/float64(theirs), "ratio")
	b.ReportMetric(float64(oursBytes), "gitproto-bytes")
	b.ReportMetric(float64(theirsBytes), "git-bytes")
}

type countingDiscard struct{ n int }

func (c *countingDiscard) Write(p []byte) (int, error) { c.n += len(p); return len(p), nil }

var _ io.Writer = (*countingDiscard)(nil)

func benchGit(b *testing.B, dir string, args ...string) string {
	b.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.Output()
	if err != nil {
		b.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}
