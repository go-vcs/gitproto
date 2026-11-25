package gitproto_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
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
	// Go heap allocations of THIS process around each side. For git's side
	// that is only the harness's os/exec driver: git itself is a C process.
	var oursAllocs, theirsAllocs uint64
	b.ResetTimer()
	b.StopTimer()
	for i := 0; i < b.N; i++ {
		var o countingDiscard
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		t0 := time.Now()
		b.StartTimer()
		if err := srv.ServeRequest(context.Background(), bytes.NewReader(req), &o); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		ours += time.Since(t0)
		runtime.ReadMemStats(&m1)
		oursAllocs += m1.Mallocs - m0.Mallocs
		oursBytes = o.n

		var g countingDiscard
		c := exec.Command("git", "-c", "pack.threads=1", "upload-pack", "--stateless-rpc", gitDir)
		c.Env = append(os.Environ(), "GIT_PROTOCOL=version=2")
		c.Stdin = bytes.NewReader(req)
		c.Stdout = &g
		runtime.ReadMemStats(&m0)
		t1 := time.Now()
		if err := c.Run(); err != nil {
			b.Fatal(err)
		}
		theirs += time.Since(t1)
		runtime.ReadMemStats(&m1)
		theirsAllocs += m1.Mallocs - m0.Mallocs
		theirsBytes = g.n
	}
	b.ReportMetric(float64(ours.Nanoseconds())/float64(b.N), "gitproto-ns/op")
	b.ReportMetric(float64(theirs.Nanoseconds())/float64(b.N), "git-ns/op")
	b.ReportMetric(float64(ours)/float64(theirs), "ratio")
	b.ReportMetric(float64(oursAllocs)/float64(b.N), "gitproto-allocs/op")
	b.ReportMetric(float64(theirsAllocs)/float64(b.N), "git-allocs/op")
	b.ReportMetric(float64(oursBytes), "gitproto-bytes")
	b.ReportMetric(float64(theirsBytes), "git-bytes")
}

type countingDiscard struct{ n int }

func (c *countingDiscard) Write(p []byte) (int, error) { c.n += len(p); return len(p), nil }

var _ io.Writer = (*countingDiscard)(nil)

// benchRepo opens the repository named by GITPROTO_BENCH_REPO.
func benchRepo(b *testing.B) (*gitproto.GitRepo, string) {
	b.Helper()
	dir := os.Getenv("GITPROTO_BENCH_REPO")
	if dir == "" {
		b.Skip("set GITPROTO_BENCH_REPO to a repository to benchmark against")
	}
	gitDir := strings.TrimSpace(benchGit(b, dir, "rev-parse", "--absolute-git-dir"))
	tip := strings.TrimSpace(benchGit(b, dir, "rev-parse", "HEAD"))
	return &gitproto.GitRepo{Dir: gitDir, PackThreads: 1}, tip
}

// BenchmarkRefs times the ref listing alone: the part of a fetch that is not
// pack generation.
func BenchmarkRefs(b *testing.B) {
	g, _ := benchRepo(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := g.Refs(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWritePack times pack generation alone.
func BenchmarkWritePack(b *testing.B) {
	g, tip := benchRepo(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var o countingDiscard
		if err := g.WritePack(context.Background(), gitproto.PackRequest{Wants: []string{tip}, OfsDelta: true}, &o); err != nil {
			b.Fatal(err)
		}
	}
}

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
