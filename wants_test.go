package gitproto_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/go-vcs/gitproto"
)

// listRepo is a Repository that only lists refs: it counts which listing a
// fetch asks for, and "serves" a one-word pack.
type listRepo struct {
	full, quick           []gitproto.Ref
	fullErr, quickErr     error
	fullCalls, quickCalls int
}

func (r *listRepo) Refs(context.Context) ([]gitproto.Ref, error) {
	r.fullCalls++
	return r.full, r.fullErr
}

func (r *listRepo) RefsUnpeeled(context.Context) ([]gitproto.Ref, error) {
	r.quickCalls++
	return r.quick, r.quickErr
}

func (r *listRepo) CommonCommits(context.Context, []string) ([]string, error) { return nil, nil }

func (r *listRepo) WritePack(_ context.Context, _ gitproto.PackRequest, w io.Writer) error {
	_, err := io.WriteString(w, "PACK")
	return err
}

// A want that is a ref's own object is checked without peeling; one that is
// only a peeled object needs the full listing; anything else is refused.
func TestWantsAreCheckedWithoutPeelingWhenPossible(t *testing.T) {
	idC := strings.Repeat("c", 40)
	idX := strings.Repeat("d", 40)
	newRepo := func() *listRepo {
		return &listRepo{
			full:  []gitproto.Ref{{Name: "refs/tags/t", ID: idB, Peeled: idC}},
			quick: []gitproto.Ref{{Name: "refs/tags/t", ID: idB}},
		}
	}
	do := func(r *listRepo, want string) error {
		_, err := serveOne(t, &gitproto.Server{Repo: r}, fetchReq("want "+want, "done"))
		return err
	}

	r := newRepo()
	if err := do(r, idB); err != nil || r.quickCalls != 1 || r.fullCalls != 0 {
		t.Fatalf("tip: err %v, quick %d, full %d", err, r.quickCalls, r.fullCalls)
	}
	r = newRepo()
	if err := do(r, idC); err != nil || r.quickCalls != 1 || r.fullCalls != 1 {
		t.Fatalf("peeled only: err %v, quick %d, full %d", err, r.quickCalls, r.fullCalls)
	}
	r = newRepo()
	err := do(r, idX)
	if err == nil || !strings.Contains(err.Error(), "not our ref "+idX) || r.fullCalls != 1 {
		t.Fatalf("not a tip: err %v, full %d", err, r.fullCalls)
	}
	// Listing errors surface, from either listing.
	boom := errors.New("boom")
	r = newRepo()
	r.quickErr = boom
	if err := do(r, idB); !errors.Is(err, boom) || r.fullCalls != 0 {
		t.Fatalf("quick listing error: %v, full %d", err, r.fullCalls)
	}
	r = newRepo()
	r.fullErr = boom
	if err := do(r, idC); !errors.Is(err, boom) {
		t.Fatalf("full listing error: %v", err)
	}
}
