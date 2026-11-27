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

## Where the last 4% went (2026-11-25, load < 8, 20 interleaved rounds)

`TestPhases` (phase_test.go, `GITPROTO_BENCH_REPO`) times each step of one
full clone, rotating the start order. Medians:

| Step                              | median ms | min ms |
|-----------------------------------|----------:|-------:|
| `Refs`                            |     13.35 |  12.60 |
| `WritePack`                       |    140.09 | 136.99 |
| `ServeRequest`                    |    154.44 | 148.16 |
| direct `pack-objects`, our args   |    140.27 | 137.91 |
| `git upload-pack`                 |    146.20 | 143.02 |

`WritePack` equals the direct `pack-objects` run, so the child, the pipe copy
and the sideband framing cost nothing measurable (the pack bytes are the same
size; a lean environment without global config changed nothing). `ServeRequest`
is `Refs` + `WritePack`: the whole gap was `Refs`. The benchmark repository has
loose refs outside `refs/heads/` (a tag, `refs/remotes/origin/*`), so the
native reader spawned one `cat-file --batch-check` (about 13 ms) to peel them,
and validating wants used that full peel. `git upload-pack` itself costs about
6 ms over its `pack-objects` child.

Fix: want validation asks `RefsUnpeeled` first, and only a want that matches
no ref object falls back to the peeled `Refs`. The accepted set is unchanged.

## After the fix (2026-11-26 08:44 CET, commit e789b10, PR #4 head)

Apple M4 Max, darwin/arm64, load 1-min 5.46 / 5-min 5.41 (under the limit of
8). Command: `GITPROTO_BENCH_REPO=<go-fleettools/fleettools clone> gotest -run
'^$' -bench FetchClone -benchmem -benchtime 20x -count 3 .`

| Run | gitproto ms/op | git ms/op | ratio  | gitproto allocs/op |
|-----|---------------:|----------:|-------:|-------------------:|
| 1   |         144.62 |    148.11 | 0.9765 | 2037               |
| 2   |         144.11 |    149.37 | 0.9647 | 2036               |
| 3   |         143.05 |    147.71 | 0.9685 | 2036               |

Pack bytes per clone: gitproto 8 419 937, git 8 427 675.

gitproto is now 2.3-3.5 % faster than `git upload-pack` on this repository.
The remaining allocations, about 2036 per request, are the next target.

## Allocations (2026-11-26, commit f215cbc then this change)

`go test -memprofile -memprofilerate 1` on `BenchmarkFetchClone` (20x) at
f215cbc, `alloc_objects`: `parsePackedRefs` 38 % flat, `strings.genSplit` (the
`strings.Split` in `validRefName`, one per ref) 20 %, `pktline.Writer.Sideband`
11 % of the objects and 93 % of the bytes (a fresh frame for each of about 270
packets, 8.4 MB per request), the rest `os` calls of the loose-ref walk.

Fixes: `Writer` reuses one frame buffer (`io.Writer` forbids retaining the
slice); `parsePackedRefs` converts the file to one string once, sizes its map
and keeps the records in one slice; `validRefName` walks the components with
`strings.Cut`. Tests: `TestWriterAllocs` (0 allocs per packet),
`TestRefParsingAllocs` (bounded independently of the ref count).

| Version | allocs/op | B/op        | gitproto ms/op | git ms/op | ratio |
|---------|----------:|------------:|---------------:|----------:|------:|
| f215cbc, 1-min load 6.51 / 5-min 5.65 | 2203 | 10 094 524 | 149.61 | 146.67 | 1.020 |
| this change, 1-min load 7.72 / 5-min 6.40 | 620 | 603 498 | 140.73 | 143.47 | 0.981 |

Both runs: 20 iterations, one run each, profile on (`-memprofilerate 1`
inflates times and the allocation counts include the profiler-visible
objects). The allocation counts are exact; the times are not an
interleaved comparison and are given only for context. 1-min load of the
second run was 7.72 and the times are not a result. The remaining 620 allocs
are `os` calls of the loose-ref walk (file open/stat/readdir) and the child
process.

## Repacked reference repository (2026-11-27, commit 4952844)

Taken 09:39-09:41 CET by the coordinator, Apple M4 Max, load 1-min 5.4-6.6 /
5-min 4.1-4.5 (under the limit of 8). Command: `GITPROTO_BENCH_REPO=<clone>
gotest -run '^$' -bench FetchClone -benchmem -benchtime 20x -count 6 .`

Input: `go-fleettools/fleettools`, which had been repacked since 2026-11-26: 5312
objects in 2 packs, 16.9 MB, 0 loose objects. It is not the repository of the
earlier tables in this file, whose numbers therefore do not compare with these.

| Run | gitproto ms/op | git upload-pack ms/op | ratio |
|-----|---------------:|----------------------:|------:|
| 1   |          85.00 |                 88.70 | 0.958 |
| 2   |          84.52 |                 88.70 | 0.953 |
| 3   |          86.77 |                 91.40 | 0.949 |
| 4   |          89.43 |                 93.89 | 0.953 |
| 5   |          88.74 |                 93.27 | 0.951 |
| 6   |          89.31 |                 96.31 | 0.927 |

gitproto 272-285 allocs/op and about 631 KB/op. Pack bytes per clone: gitproto
8 418 351, git 8 425 958. gitproto is 4-7 % faster than `git upload-pack` in
every run (the benchmark interleaves the two).
