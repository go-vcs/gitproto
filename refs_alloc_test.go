package gitproto

import (
	"fmt"
	"strings"
	"testing"
)

// TestRefParsingAllocs pins the allocations of the ref parsers: they were one
// per packed-refs line (several per ref) and one per path component of every
// ref name; now the file is one string and the records one slice.
func TestRefParsingAllocs(t *testing.T) {
	const refs = 500
	var sb strings.Builder
	sb.WriteString("# pack-refs with: peeled fully-peeled sorted \n")
	for i := range refs {
		fmt.Fprintf(&sb, "%040x refs/tags/v%04d\n^%040x\n", i+1, i, i+2)
	}
	file := []byte(sb.String())

	got, err := parsePackedRefs(file)
	if err != nil || len(got) != refs {
		t.Fatalf("parsePackedRefs: %d refs, %v", len(got), err)
	}
	if r := got["refs/tags/v0007"]; r == nil || r.id != fmt.Sprintf("%040x", 8) || r.peeled != fmt.Sprintf("%040x", 9) {
		t.Fatalf("v0007 = %+v", r)
	}
	// File string, map (with its buckets), record slice, and a little more:
	// bounded independently of the number of refs.
	if n := testing.AllocsPerRun(5, func() { _, _ = parsePackedRefs(file) }); n > 40 {
		t.Errorf("parsePackedRefs: %v allocs for %d refs, want <= 40", n, refs)
	}

	if n := testing.AllocsPerRun(50, func() { _ = validRefName("refs/remotes/origin/feature/x") }); n != 0 {
		t.Errorf("validRefName: %v allocs, want 0", n)
	}
	for name, want := range map[string]bool{
		"refs/heads/main": true, "refs//x": false, "refs/.x/y": false,
		"refs/a.lock/y": false, "refs/a/": false, "refs/a/b.lock": false, "refs/a/b": true,
	} {
		if validRefName(name) != want {
			t.Errorf("validRefName(%q) = %v, want %v", name, !want, want)
		}
	}
}
