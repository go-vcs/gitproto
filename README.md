# gitproto

Git wire protocol v2 (fetch side) as a Go library, with no knowledge of any
forge: it takes an `io.Reader`, an `io.Writer` and a `Repository`, so a forge
can mount it on SSH and on smart HTTP. Part of [go-vcs](https://github.com/go-vcs),
a light, secure Git forge.

Status: **milestone 1** — serving `ls-refs` and `fetch` from an on-disk
repository. No push, no shallow, no filters.

## Use

```go
srv := &gitproto.Server{Repo: &gitproto.GitRepo{Dir: "/srv/git/x.git"}}

// Stateful transports (ssh, stdio, git://): advertise, then loop.
err := srv.Serve(ctx, conn, conn)

// Smart HTTP: GET info/refs -> Advertise; POST git-upload-pack -> ServeRequest.
err := srv.Advertise(w)
err := srv.ServeRequest(ctx, r, w) // io.EOF = empty request
```

A refused request is answered with an `ERR <reason>` pkt-line and returned as
`*gitproto.ProtocolError`; repository failures are answered with the generic
`ERR internal error` (paths and git's stderr never reach the client) and
returned to the caller as is.

`cmd/gitproto-upload-pack` serves `git-upload-pack` over stdio (cobra), so the
real git client can drive it:

```
git -c protocol.version=2 clone --upload-pack=gitproto-upload-pack file:///srv/git/x.git
```

It requires `GIT_PROTOCOL=version=2` and refuses protocol 0 and 1.
`--advertise-refs` and `--stateless-rpc` give the smart-HTTP halves.

## Object storage: delegated to `git`, behind one interface

`Repository` is the narrow seam (`Refs`, `CommonCommits`, `WritePack`).
Milestone 1 implements it with the `git` binary (`GitRepo`): `for-each-ref`,
`symbolic-ref`, `rev-parse`, `cat-file --batch-check`, and
`pack-objects --revs --stdout`. Every command line is fixed in
`gitrepo.go`; no wire input reaches argv (object ids are validated as 40
lowercase hex digits and written to stdin), no shell is involved, and git runs
with a minimal environment (`GIT_DIR`, no user or system config, no replace
objects). A pure-Go object store is a later decision, to be taken from
measurements; it only has to implement `Repository`.

## What is supported, what is refused

Supported: capability advertisement (`version 2`, `agent`, `ls-refs=unborn`,
`fetch`, `object-format=sha1`); `ls-refs` with `peel`, `symrefs`, `ref-prefix`,
`unborn`; `fetch` with `want`, `have`, `done`, `thin-pack`, `no-progress`,
`include-tag`, `ofs-delta`, acknowledgments (`ACK`/`NAK`/`ready`) and the
side-band-64k packfile section.

Refused explicitly, each with a test (`server_test.go`, `TestRefusals`,
`TestLimits`, `pktline_test.go`):

- pkt-lines: length not four lowercase hex digits, `0003`, longer than 65520
  bytes (refused after the 4 header bytes, before any payload is read),
  truncated packets, NUL in a request line;
- request shape: no `command=`, invalid command or capability names, delim or
  response-end packets where they do not belong, a second delim, truncation;
- capabilities not advertised: `server-option`, unknown keys, any
  `object-format` but `sha1`, empty / non-printable / duplicated `agent`;
- commands not advertised: `object-info`, `bundle-uri`, anything else;
- `ls-refs` arguments the spec does not define;
- `fetch` features not advertised, by name: `shallow`, `deepen`,
  `deepen-relative`, `deepen-since`, `deepen-not`, `filter`, `want-ref`,
  `sideband-all`, `packfile-uris`, `wait-for-done`; unknown arguments; malformed
  object ids; values on flag arguments;
- a `want` that is not an advertised tip (or the peeled object of one):
  `not our ref`. Unlike a bare `git upload-pack`, an object is never served
  just because the client can name it;
- bounded requests (`Limits`, defaults 8 MiB, 100 000 lines, 50 000 wants and
  haves, 1 024 ref-prefixes); a request that never ends is cut off at the limit
  (`TestBomb*`, run under `GOMEMLIMIT=64MiB` in CI).

Fuzz targets: `FuzzReader` (pkt-line), `FuzzParseRequest`, `FuzzParseFetch`,
`FuzzParseLsRefs`, `FuzzServe` (whole server over a fake repository). Seed
corpus under `testdata/fuzz`; CI runs each target for 20 s.

Real `git` is the oracle: `ls-refs` output is compared byte for byte with
`git upload-pack --stateless-rpc` on six repository shapes (normal, empty,
detached HEAD, HEAD at a tag, symbolic ref); packs are checked with
`git index-pack --strict --fix-thin` and `git fsck`; `git clone` and
`git fetch` run through the built binary.

## Benchmark

See [BENCHMARK.md](BENCHMARK.md).

## Bibliography

Opened and used (git v2.47.0, `Documentation/`):

- `gitprotocol-v2` — capability advertisement, command request, `ls-refs`,
  `fetch` sections, special packets 0000/0001/0002;
- `gitprotocol-common` — pkt-line format, the 65520 / 65516 bounds, flush-pkt
  versus empty pkt-line;
- `gitprotocol-pack` — side-band(-64k) framing of the packfile (lines 472–490);
- `gitprotocol-capabilities` — `side-band-64k` (lines 135–168).

`gitprotocol-v2` also describes `shallow`, `filter`, `ref-in-want`,
`sideband-all`, `packfile-uris`, `wait-for-done`, `object-info`, `bundle-uri`
and `server-option`: read, not implemented, refused.

## License

BSD-3-Clause.
