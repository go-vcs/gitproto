# Benchmark: gitproto versus `git upload-pack`

`BenchmarkFetchClone` (bench_test.go) serves one full-clone `fetch` request
(`want <HEAD>`, `ofs-delta`, `done`) through `Server.ServeRequest` with
`PackThreads: 1`, and through `git -c pack.threads=1 upload-pack
--stateless-rpc` with `GIT_PROTOCOL=version=2`. The two sides alternate inside
each iteration, each with its own timer, so load drift hits both. Output goes
to a counting discard writer; the byte counts are reported to show both sides
sent a pack of the same size.

```
GITPROTO_BENCH_REPO=/path/to/repo go test -run '^$' -bench FetchClone -benchtime 20x .
```

## Result (2026-11-25, Apple M4 Max, git on PATH, one pack thread)

Repository: a 8.4 MB-pack clone of go-fleettools/fleettools (about 170 tags).

| Version of the Refs path                    | gitproto ns/op | git ns/op   | ratio |
|---------------------------------------------|---------------:|------------:|------:|
| one `rev-parse` process per annotated tag   |    376 896 842 | 149 864 592 | 2.52  |
| one `cat-file --batch-check` for all tags   |    205 841 021 | 152 475 398 | 1.35  |

Pack sizes: gitproto 8 419 937 bytes, git 8 427 705 bytes.

**Measured at a 1-minute load of about 9**, above the repository's limit of 8:
the ratios are indicative, not a reference measurement. Rerun when the load is
below 8 before quoting them. Allocation counts were not measured.

## Open defect

gitproto is still slower than `git upload-pack` (1.35x): the repository
interface costs three extra short-lived `git` processes per request
(`symbolic-ref HEAD`, `rev-parse HEAD`, `rev-parse HEAD^{}`) that
`upload-pack` does in process. Next step: fold the HEAD questions into the one
`cat-file --batch-check` call, then measure again.
