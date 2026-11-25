package gitproto

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var (
	oidA = strings.Repeat("a", 40)
	oidB = strings.Repeat("b", 40)
)

func TestParsePackedRefs(t *testing.T) {
	hdr := "# pack-refs with: peeled fully-peeled sorted \n"
	got, err := parsePackedRefs([]byte(hdr + oidA + " refs/heads/main\n" + oidB + " refs/tags/v1\n^" + oidA + "\n"))
	if err != nil || len(got) != 2 || got["refs/tags/v1"].peeled != oidA || got["refs/heads/main"].peeled != "" {
		t.Fatalf("%+v %v", got, err)
	}
	if got, err := parsePackedRefs(nil); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
	unsupported := map[string]string{
		"not fully peeled":     "# pack-refs with: peeled sorted \n" + oidA + " refs/heads/a\n",
		"no header":            oidA + " refs/heads/a\n",
		"bad header":           "# something else\n" + oidA + " refs/heads/a\n",
		"header w/o newline":   "# pack-refs with: fully-peeled",
		"unterminated line":    hdr + oidA + " refs/heads/a",
		"short id":             hdr + "abc refs/heads/a\n",
		"upper-case id":        hdr + strings.Repeat("A", 40) + " refs/heads/a\n",
		"peel line first":      hdr + "^" + oidA + "\n",
		"two peel lines":       hdr + oidA + " refs/tags/t\n^" + oidA + "\n^" + oidB + "\n",
		"bad peel id":          hdr + oidA + " refs/tags/t\n^zz\n",
		"duplicate":            hdr + oidA + " refs/heads/a\n" + oidB + " refs/heads/a\n",
		"name outside refs/":   hdr + oidA + " HEAD\n",
		"name with space-dots": hdr + oidA + " refs/heads/a..b\n",
		"name ending .lock":    hdr + oidA + " refs/heads/a.lock\n",
		"name with NUL":        hdr + oidA + " refs/heads/a\x00b\n",
	}
	for name, in := range unsupported {
		if _, err := parsePackedRefs([]byte(in)); !errors.Is(err, errUnsupported) {
			t.Errorf("%s: got %v, want errUnsupported", name, err)
		}
	}
}

func TestParsePackedRefsBounds(t *testing.T) {
	hdr := "# pack-refs with: peeled fully-peeled sorted \n"
	old := maxRefCount
	maxRefCount = 2
	defer func() { maxRefCount = old }()
	in := hdr + oidA + " refs/heads/a\n" + oidA + " refs/heads/b\n" + oidA + " refs/heads/c\n"
	var eb *errRefBound
	if _, err := parsePackedRefs([]byte(in)); !errors.As(err, &eb) {
		t.Fatalf("ref count: %v", err)
	}
	long := hdr + oidA + " refs/heads/" + strings.Repeat("x", maxRefLineBytes) + "\n"
	if _, err := parsePackedRefs([]byte(long)); !errors.As(err, &eb) {
		t.Fatalf("line length: %v", err)
	}
}

func TestConfigIsFilesBackend(t *testing.T) {
	yes := []string{
		"",
		"[core]\n\tbare = true\n",
		"[core]\n\trepositoryformatversion = 1\n[extensions]\n\trefStorage = files\n",
		"[extensions]\n\trefstorage = \"files\"\n",
		"[extensions]\n\tobjectformat = sha1\n",
	}
	no := []string{
		"[extensions]\n\trefStorage = reftable\n",
		"[extensions]\n\trefstorage = future-backend\n",
		"[EXTENSIONS]\n\tREFSTORAGE = Reftable\n",
		"[extensions]\n\trefStorage = files\n\trefStorage = reftable\n",
		"[extensions]\n\trefStorage\n",
		"[extensions]\n\trefStorage = files # reftable\n",
		"[core]\n\trefStorage = files\n", // not under extensions: refuse to guess
		"[include]\n\tpath = other\n",
		"[includeIf \"gitdir:/x\"]\n\tpath = other\n",
		"[extensions]\nrefStorage = \\\nreftable\n",
	}
	for _, c := range yes {
		if !configIsFilesBackend([]byte(c)) {
			t.Errorf("files backend refused: %q", c)
		}
	}
	for _, c := range no {
		if configIsFilesBackend([]byte(c)) {
			t.Errorf("accepted as files backend: %q", c)
		}
	}
}

func TestValidRefName(t *testing.T) {
	for _, n := range []string{"refs/heads/main", "refs/tags/v1.0", "refs/a/b/c", "refs/heads/ünï"} {
		if !validRefName(n) {
			t.Errorf("rejected %q", n)
		}
	}
	for _, n := range []string{"", "@", "refs/", "refs//x", "refs/.x", "refs/x.", "refs/x.lock", "refs/x/y.lock/z", "refs/a..b", "refs/a@{b", "refs/a b", "refs/a~b", "refs/a^b", "refs/a:b", "refs/a?b", "refs/a*b", "refs/a[b", "refs/a\\b", "refs/a\tb", "refs/a\x7fb"} {
		if validRefName(n) {
			t.Errorf("accepted %q", n)
		}
	}
}

// The loose-ref reader's bounds are refusals, not fallbacks.
func TestLooseRefBounds(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "refs", "heads"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "config"), []byte("[core]\n"), 0o644))
	big := filepath.Join(dir, "refs", "heads", "big")
	must(os.WriteFile(big, []byte(strings.Repeat("x", maxRefLineBytes+1)), 0o644))
	g := &GitRepo{Dir: dir, Git: filepath.Join(dir, "no-such-git")}
	var eb *errRefBound
	if _, err := g.Refs(t.Context()); !errors.As(err, &eb) {
		t.Fatalf("oversized loose ref: %v", err)
	}
	must(os.Remove(big))

	// A directory chain deeper than the bound.
	p := filepath.Join(dir, "refs")
	for range maxRefDirDepth + 2 {
		p = filepath.Join(p, "d")
	}
	must(os.MkdirAll(p, 0o755))
	if _, err := g.Refs(t.Context()); !errors.As(err, &eb) {
		t.Fatalf("deep refs/: %v", err)
	}
	must(os.RemoveAll(filepath.Join(dir, "refs", "d")))

	// An oversized packed-refs is refused from its size alone.
	pk := filepath.Join(dir, "packed-refs")
	f, err := os.Create(pk)
	must(err)
	must(f.Truncate(int64(maxPackedRefsBytes) + 1))
	must(f.Close())
	if _, err := g.Refs(t.Context()); !errors.As(err, &eb) {
		t.Fatalf("oversized packed-refs: %v", err)
	}
	must(os.Remove(pk))

	// Nothing is wrong now: an unborn HEAD, no refs, answered without git.
	got, err := g.Refs(t.Context())
	if err != nil || len(got) != 1 || got[0].Name != "HEAD" || got[0].ID != "" || got[0].Target != "refs/heads/main" {
		t.Fatalf("%+v %v", got, err)
	}

	// Symbolic ref chains: a cycle and an over-long chain go to git (which
	// is not there), they are not guessed at.
	must(os.WriteFile(filepath.Join(dir, "refs", "heads", "a"), []byte("ref: refs/heads/b\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "refs", "heads", "b"), []byte("ref: refs/heads/a\n"), 0o644))
	if _, err := g.refsNative(t.Context()); !errors.Is(err, errUnsupported) {
		t.Fatalf("symref cycle: %v", err)
	}
}

func FuzzParsePackedRefs(f *testing.F) {
	hdr := "# pack-refs with: peeled fully-peeled sorted \n"
	f.Add([]byte(""))
	f.Add([]byte(hdr))
	f.Add([]byte(hdr + oidA + " refs/heads/main\n"))
	f.Add([]byte(hdr + oidA + " refs/heads/main\n" + oidB + " refs/tags/v1\n^" + oidA + "\n"))
	f.Add([]byte(hdr + oidA + " refs/heads/main"))
	f.Add([]byte("^" + oidA + "\n"))
	f.Add([]byte("# pack-refs with: \n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		got, err := parsePackedRefs(b)
		if err != nil {
			if !errors.Is(err, errUnsupported) {
				var eb *errRefBound
				if !errors.As(err, &eb) {
					t.Fatalf("unclassified error %v", err)
				}
			}
			return
		}
		if len(got) > maxRefCount {
			t.Fatalf("%d refs", len(got))
		}
		for name, r := range got {
			if !validRefName(name) || !strings.HasPrefix(name, "refs/") || !ValidOID(r.id) || r.target != "" || r.loose {
				t.Fatalf("accepted %q %+v", name, r)
			}
			if r.peeled != "" && !ValidOID(r.peeled) {
				t.Fatalf("accepted peel %q for %q", r.peeled, name)
			}
		}
	})
}
