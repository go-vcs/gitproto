package gitproto

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Reading refs without a process.
//
// A fetch must know the advertised tips before it generates a pack, and a
// `git for-each-ref` process for that costs about as much as everything else
// in the request. This file reads the "files" ref backend directly:
//
//   - gitrepository-layout(5): refs/ holds one file per ref, packed-refs
//     "records the same information ... in a more efficient way", and HEAD is
//     "a symref ... to the refs/heads/ namespace", or a detached object id.
//   - packed-refs, as git's refs/packed-backend.c reads it: an optional
//     header "# pack-refs with: " followed by space separated traits;
//     "fully-peeled" means every peelable ref has a "^<oid>" line right after
//     it and a ref without one is not peelable; records are
//     "<40 hex> <refname>\n"; the file is sorted when "sorted" is a trait.
//   - A loose ref overrides the packed one of the same name.
//   - Which backend a repository uses is its config: extensions.refStorage
//     ("files" or "reftable"; the key is absent for files). A reftable
//     repository keeps its refs in reftable/ and a stub HEAD, so reading
//     refs/ and packed-refs there would answer "no refs" without an error.
//
// Anything this reader does not fully understand is not guessed at: it
// returns errUnsupported and Refs asks git, which is the oracle. Anything
// over a bound is refused with errRefBound.
var errUnsupported = errors.New("gitproto: ref layout not read natively")

// errRefBound is returned when a ref store exceeds a bound; the request is
// refused, not delegated (git would read the whole of it).
type errRefBound struct{ what string }

func (e *errRefBound) Error() string { return "gitproto: ref store too large: " + e.what }

// The bounds are variables only so that tests can lower them.
var (
	maxPackedRefsBytes = 256 << 20 // size of packed-refs
	maxRefLineBytes    = 8 << 10   // one packed-refs line, or one loose ref file
	maxConfigBytes     = 1 << 20   // size of the config read for extensions.refStorage
	maxRefCount        = 1 << 20   // refs, loose and packed together
	maxRefDirDepth     = 64        // directory levels under refs/
	maxSymrefDepth     = 5         // git's own bound on symbolic ref chains

	osReadFile = os.ReadFile // replaced by tests to inject a read error or a growing file
)

// rawRef is one ref as stored.
type rawRef struct {
	id     string // object id, "" for a symbolic ref until resolved
	target string // direct target of a symbolic ref
	peeled string // fully peeled id, when the store records one
	loose  bool
}

// refsNative lists the refs of a "files" repository without a process, or
// returns errUnsupported when the repository is not one this reader handles.
func (g *GitRepo) refsNative(ctx context.Context, wantPeel bool) ([]Ref, error) {
	if err := g.checkFilesBackend(); err != nil {
		return nil, err
	}
	head, err := readRefFile(filepath.Join(g.Dir, "HEAD"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errUnsupported // no HEAD: not a repository this reader knows
	}
	if err != nil {
		return nil, err
	}
	// Anything but "ref: refs/..." (a detached HEAD, garbage) is git's to judge.
	headTarget, headID, _ := parseRefContent(head)
	if headTarget == "" || headID != "" {
		return nil, errUnsupported
	}

	all := map[string]*rawRef{}
	// Loose refs first, packed second: pack-refs writes the packed file
	// before it deletes the loose ones, so this order never misses a ref
	// that is being packed.
	if err := walkLoose(ctx, filepath.Join(g.Dir, "refs"), "refs", 0, all); err != nil {
		return nil, err
	}
	packed, err := readPacked(filepath.Join(g.Dir, "packed-refs"))
	if err != nil {
		return nil, err
	}
	for name, p := range packed {
		if _, ok := all[name]; !ok {
			if len(all) >= maxRefCount {
				return nil, &errRefBound{"more than a million refs"}
			}
			all[name] = p
		}
	}

	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	slices.Sort(names) // bytewise, as for-each-ref's refname order

	// resolve follows symbolic refs to the ref that holds an id.
	resolve := func(name string) (rawRef, string, error) {
		r := all[name]
		for depth := 0; r != nil && r.target != ""; depth++ {
			if depth >= maxSymrefDepth {
				return rawRef{}, "", errUnsupported
			}
			name = r.target
			r = all[name]
		}
		var fin rawRef
		if r != nil {
			fin = *r
		}
		if r == nil {
			return fin, "", errUnsupported // a dangling symbolic ref: git decides
		}
		return fin, name, nil
	}

	refs := make([]Ref, 0, len(names)+1)
	var peel []int // refs[i] needs git to peel it
	var in strings.Builder
	for _, n := range names {
		fin, finName, err := resolve(n)
		if err != nil {
			return nil, err
		}
		ref := Ref{Name: n, ID: fin.id, Peeled: fin.peeled}
		if all[n].target != "" {
			ref.Target = finName // git reports the end of a symref chain
		}
		// A loose ref outside refs/heads/ may name a tag object, and a loose
		// file does not say. (git refuses to write a non-commit under
		// refs/heads/, so those are commits.)
		if fin.loose && !strings.HasPrefix(finName, "refs/heads/") {
			peel = append(peel, len(refs))
			in.WriteString(fin.id + "^{}\n")
		}
		refs = append(refs, ref)
	}
	if wantPeel && len(peel) > 0 {
		lines, err := g.catFileLines(ctx, in.String(), len(peel))
		if err != nil {
			return nil, err
		}
		for k, i := range peel {
			ans := ""
			if k < len(lines) {
				ans = lines[k]
			}
			if !ValidOID(ans) {
				return nil, errUnsupported // missing or odd: git decides
			}
			if ans != refs[i].ID {
				refs[i].Peeled = ans
			}
		}
	}

	h := Ref{Name: "HEAD", Target: headTarget}
	if i, ok := slices.BinarySearch(names, headTarget); ok {
		// HEAD names a listed ref: its id and peel are that ref's.
		h.ID, h.Peeled = refs[i].ID, refs[i].Peeled
	}
	// Otherwise HEAD is unborn: no id, as for a symbolic-ref to nothing.
	return slices.Insert(refs, 0, h), nil
}

// checkFilesBackend refuses (errUnsupported) a repository that is not a plain
// "files" one: a reftable or unknown extensions.refStorage, a linked worktree
// (commondir), a reftable directory, an include in the config.
func (g *GitRepo) checkFilesBackend() error {
	for _, p := range []string{"commondir", "reftable"} {
		if _, err := os.Lstat(filepath.Join(g.Dir, p)); !errors.Is(err, fs.ErrNotExist) {
			return errUnsupported // present, or unreadable: not "plain files"
		}
	}
	cfg, err := readBounded(filepath.Join(g.Dir, "config"), int64(maxConfigBytes))
	if err != nil || !configIsFilesBackend(cfg) {
		return errUnsupported
	}
	return nil
}

// configIsFilesBackend reports whether the config says the files backend:
// no extensions.refStorage at all, or exactly "files". Any mention of
// refStorage that is not parsed as exactly that, and any include, is "no".
func configIsFilesBackend(cfg []byte) bool {
	low := bytes.ToLower(cfg)
	if bytes.Contains(low, []byte("[include")) {
		return false
	}
	if !bytes.Contains(low, []byte("refstorage")) {
		return true
	}
	section := ""
	found := ""
	n := 0
	for line := range bytes.SplitSeq(low, []byte("\n")) {
		s := strings.TrimSpace(string(line))
		switch {
		case strings.HasPrefix(s, "["):
			// An unclosed "[name" gets section "", which counts no refStorage
			// key and so answers no below.
			end := strings.IndexAny(s, "] \t\"")
			section = s[1:max(end, 1)]
		case section == "extensions" && strings.HasPrefix(s, "refstorage"):
			k, v, ok := strings.Cut(s, "=")
			_ = ok // no "=": k is the whole line, found stays empty, the answer is no
			if strings.TrimSpace(k) != "refstorage" {
				return false
			}
			found = strings.Trim(strings.TrimSpace(v), `"`)
			n++
		}
	}
	return n == 1 && found == "files" && bytes.Count(low, []byte("refstorage")) == 1
}

// readBounded reads a regular file of at most max bytes.
func readBounded(path string, max int64) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errUnsupported
	}
	if fi.Size() > max {
		return nil, &errRefBound{filepath.Base(path) + " larger than the bound"}
	}
	b, err := osReadFile(path)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, &errRefBound{filepath.Base(path) + " grew past the bound"}
	}
	return b, nil
}

func readRefFile(path string) ([]byte, error) {
	b, err := readBounded(path, int64(maxRefLineBytes))
	if err != nil {
		var eb *errRefBound
		if errors.As(err, &eb) {
			return nil, err
		}
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err // vanished: the caller decides whether that matters
		}
		return nil, errUnsupported
	}
	return b, nil
}

// parseRefContent parses a loose ref or HEAD file: "ref: <name>" gives a
// target, a 40 hex digit id gives an id.
func parseRefContent(b []byte) (target, id string, err error) {
	s := strings.TrimSuffix(string(b), "\n")
	if rest, ok := strings.CutPrefix(s, "ref: "); ok {
		rest = strings.TrimSpace(rest)
		if !validRefName(rest) || !strings.HasPrefix(rest, "refs/") {
			return "", "", errUnsupported
		}
		return rest, "", nil
	}
	if ValidOID(s) {
		return "", s, nil
	}
	return "", "", errUnsupported
}

// validRefName is a conservative subset of git check-ref-format: a name it
// rejects is handed to git, never accepted.
func validRefName(n string) bool {
	if n == "" || n == "@" || strings.HasSuffix(n, "/") || strings.HasSuffix(n, ".") ||
		strings.Contains(n, "..") || strings.Contains(n, "@{") || strings.Contains(n, "//") {
		return false
	}
	// Each "/"-separated component; walked in place, no Split allocation.
	for rest, more := n, true; more; {
		var c string
		c, rest, more = strings.Cut(rest, "/")
		if c == "" || c[0] == '.' || strings.HasSuffix(c, ".lock") {
			return false
		}
	}
	for i := 0; i < len(n); i++ {
		switch c := n[i]; {
		case c < 0x20 || c == 0x7f:
			return false
		case strings.IndexByte(" ~^:?*[\\", c) >= 0:
			return false
		}
	}
	return true
}

// walkLoose reads the loose refs under dir (named prefix) into all.
func walkLoose(ctx context.Context, dir, prefix string, depth int, all map[string]*rawRef) error {
	if depth > maxRefDirDepth {
		return &errRefBound{"refs/ nested deeper than the bound"}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if depth > 0 && errors.Is(err, fs.ErrNotExist) {
			return nil // packed away while we walked
		}
		return errUnsupported
	}
	for _, e := range ents {
		name := prefix + "/" + e.Name()
		path := filepath.Join(dir, e.Name())
		switch {
		case e.IsDir():
			if err := walkLoose(ctx, path, name, depth+1, all); err != nil {
				return err
			}
		case strings.HasSuffix(e.Name(), ".lock"):
			continue // git's own lock files are not refs
		case e.Type().IsRegular():
			if !validRefName(name) {
				return errUnsupported
			}
			b, err := readRefFile(path)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return err
			}
			target, id, err := parseRefContent(b)
			if err != nil {
				return err
			}
			if len(all) >= maxRefCount {
				return &errRefBound{"more than a million refs"}
			}
			all[name] = &rawRef{id: id, target: target, loose: true}
		default:
			return errUnsupported // a symlink or special file
		}
	}
	return nil
}

// readPacked reads packed-refs; a missing file is no packed refs.
func readPacked(path string) (map[string]*rawRef, error) {
	b, err := readBounded(path, int64(maxPackedRefsBytes))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return parsePackedRefs(b)
}

// parsePackedRefs parses the contents of a packed-refs file. A file git would
// call malformed, unsorted-by-claim or not fully peeled is errUnsupported.
func parsePackedRefs(b []byte) (map[string]*rawRef, error) {
	// One string for the whole file: every id and name below is a substring
	// of it, instead of one allocation per line. The refs keep it alive, and
	// the file is bounded (maxPackedRefsBytes).
	s := string(b)
	// A ref line is about 60 bytes; sizing the map and the records up front
	// avoids growth and one allocation per ref.
	est := min(strings.Count(s, "\n"), maxRefCount)
	out := make(map[string]*rawRef, est)
	recs := make([]rawRef, 0, est)
	fully := false
	if len(s) > 0 && s[0] == '#' {
		hdr, rest, found := strings.Cut(s, "\n")
		if !found || len(hdr) > maxRefLineBytes {
			return nil, errUnsupported
		}
		traits, ok := strings.CutPrefix(hdr, "# pack-refs with: ")
		if !ok {
			return nil, errUnsupported
		}
		for t := range strings.SplitSeq(traits, " ") {
			if t == "fully-peeled" {
				fully = true
			}
		}
		s = rest
	}
	// last starts as a record that already has a peel line, so a "^" line
	// before any ref is refused by the same test as a second one.
	last := &rawRef{peeled: "-"}
	for len(s) > 0 {
		line, rest, found := strings.Cut(s, "\n")
		if !found {
			return nil, errUnsupported // an unterminated last line
		}
		if len(line) > maxRefLineBytes {
			return nil, &errRefBound{"a packed-refs line longer than the bound"}
		}
		s = rest
		if rest, ok := strings.CutPrefix(line, "^"); ok {
			if last.peeled != "" || !ValidOID(rest) {
				return nil, errUnsupported
			}
			last.peeled = rest
			continue
		}
		id, name, ok := strings.Cut(line, " ")
		if !ok || !ValidOID(id) || !validRefName(name) || !strings.HasPrefix(name, "refs/") {
			return nil, errUnsupported
		}
		if _, dup := out[name]; dup {
			return nil, errUnsupported
		}
		if len(out) >= maxRefCount {
			return nil, &errRefBound{"more than a million refs"}
		}
		recs = append(recs, rawRef{id: id}) // cap est >= refs: never reallocates
		last = &recs[len(recs)-1]
		out[name] = last
	}
	if !fully && len(out) > 0 {
		return nil, errUnsupported // peeled-ness of a ref is unknown
	}
	return out, nil
}

// catFileLines runs one cat-file --batch-check over in (n lines) and returns
// the n answers.
func (g *GitRepo) catFileLines(ctx context.Context, in string, n int) ([]string, error) {
	out, err := g.run(ctx, in, "cat-file", "--batch-check=%(objectname)")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(lines) != n {
		return nil, fmt.Errorf("gitproto: cat-file answered %d lines for %d refs", len(lines), n)
	}
	return lines, nil
}
