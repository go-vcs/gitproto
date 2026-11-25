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
| `Refs` read in Go from the files backend (17:36, load 4.24 / 4.77, 3 runs of 20) | 153 311 827 / 150 864 948 / 152 758 890 | 146 064 327 / 144 926 225 / 146 695 665 | 1.050 / 1.041 / 1.041 | 2033 / 2031 / 2030 | 45 |

Pack sizes: gitproto 8 419 937 bytes, git 8 427 705 / 8 427 740 bytes.

The last row was taken at a load of 4.2 (under the limit of 8) and replaces
the 1.074 row as the reference; the rows above it were taken at load 8 to 9.
`Refs` now reads HEAD, `refs/` and `packed-refs` itself for a plain `files`
repository, and spawns `git` only for a loose ref outside `refs/heads/` (to
peel it) or for a layout it does not read (reftable, detached HEAD, linked
worktree, an include in the config, a dangling symref). The extra allocations
per request are the parse of about 170 refs in Go; the `git` column counts only
the `os/exec` driver.

## Open defect

gitproto is still about 4% (6 ms) slower than `git upload-pack`. The
remainder was not profiled: the Go process is on-CPU for a few percent of the
wall time, so it is spent in the `pack-objects` child and the request handling
around it.
