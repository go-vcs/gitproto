package gitproto

import (
	"context"
	"io"
)

// Ref is one reference as ls-refs reports it.
type Ref struct {
	// Name is the full ref name, or "HEAD".
	Name string
	// ID is the object id the ref resolves to (40 lowercase hex digits).
	// It is empty only for an unborn HEAD.
	ID string
	// Target is the name of the ref this symbolic ref points to, or "".
	Target string
	// Peeled is the fully peeled object id when ID is an annotated tag.
	Peeled string
}

// PackRequest is what fetch asks of the object store. Every id has already
// been validated as 40 lowercase hex digits by the protocol layer.
type PackRequest struct {
	// Wants are the objects to send, with everything reachable from them.
	Wants []string
	// Common are commits the client has; everything reachable from them is
	// left out.
	Common []string
	// Thin permits deltas against objects the client has.
	Thin bool
	// OfsDelta permits OBJ_OFS_DELTA.
	OfsDelta bool
	// IncludeTag adds annotated tags that point at sent objects.
	IncludeTag bool
}

// Repository is the narrow interface between the protocol and an object
// store. The git-binary-backed implementation is GitRepo; a pure-Go store can
// implement it later without touching the protocol code.
type Repository interface {
	// Refs lists HEAD (first, possibly unborn) and every ref, sorted by
	// name, with symbolic targets and peeled ids.
	Refs(ctx context.Context) ([]Ref, error)
	// CommonCommits returns, in input order, those of ids that are commits
	// present in the repository.
	CommonCommits(ctx context.Context, ids []string) ([]string, error)
	// WritePack writes a pack file for req to w.
	WritePack(ctx context.Context, req PackRequest, w io.Writer) error
}

// ValidOID reports whether s is 40 lowercase hexadecimal digits.
func ValidOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
