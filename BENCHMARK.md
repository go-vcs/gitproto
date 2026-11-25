# Benchmark: gitproto versus `git upload-pack`

`BenchmarkFetchClone` (bench_test.go) serves one full-clone `fetch` request
(`want <HEAD>`, `ofs-delta`, `done`) through `Server.ServeRequest` with
`PackThreads: 1`, and through `git -c pack.threads=1 upload-pack
--stateless-rpc` with `GIT_PROTOCOL=version=2`. The two sides alternate inside
each iteration, each with its own timer, so load drift hits both. Output goes
to a counting discard writer; the byte counts are reported to show both sides
sent a pack of the same size. Allocations are the Go heap allocations of the
benchmark process around each side (`runtime.MemStats.Mallocs`): for git's
side that is only the harness's `os/exec` driver, since git is a C process.

```
GITPROTO_BENCH_REPO=/path/to/repo gotest -run '^$' -bench FetchClone -benchtime 20x .
```

`BenchmarkRefs` and `BenchmarkWritePack` time the two halves of a fetch alone.

## Profile (2026-11-25)

The Go process is on-CPU for about 2% of the wall time of `Refs`; everything
else is waiting for child processes. The extra time over upload-pack was the
five serial `git` spawns of `Refs` (`for-each-ref`, `cat-file`,
`symbolic-ref`, `rev-parse HEAD`, `rev-parse HEAD^{}`), each about 10 ms,
which upload-pack does in process.

## Result (2026-11-25 16:36, Apple M4 Max, git on PATH, one pack thread)

Repository: an 8.4 MB-pack clone of go-fleettools/fleettools (about 170 tags).
Load at the run: 1-min 7.97, 5-min 7.89 (under the limit of 8; borderline).
20 iterations, interleaved.

| Version of the Refs path                       | gitproto ns/op | git ns/op   | ratio | gitproto allocs/op | git allocs/op |
|------------------------------------------------|---------------:|------------:|------:|-------------------:|--------------:|
| one `rev-parse` per tag (load ~9, indicative)  |    376 896 842 | 149 864 592 | 2.52  | not measured       | not measured  |
| one `cat-file` for all tags (load ~9, indic.)  |    205 841 021 | 152 475 398 | 1.35  | not measured       | not measured  |
| concurrent `for-each-ref` + `symbolic-ref`, no `cat-file` unless tag-of-tag, HEAD from the listing | 157 305 915 | 146 493 033 | 1.074 | 1486 | 45 |

Pack sizes: gitproto 8 419 937 bytes, git 8 427 705 / 8 427 740 bytes.

## Open defect

gitproto is still 7% (about 11 ms) slower than `git upload-pack`. Remaining
cause: two `git` processes per request (`for-each-ref` to validate the wants,
then `pack-objects`) where upload-pack spawns only `pack-objects` and reads
refs in process. Removing it means reading refs in Go (loose and packed refs,
reftable) instead of through git, or running `pack-objects` before the wants
are validated, which this server refuses to do. The rows above the last were
taken at a load of about 9 and are indicative only. The 1486 allocations per
request are mostly `os/exec` plumbing and the ref parsing for ~170 refs; they
were not profiled further because the time is spent waiting on children.
