package gitproto

import (
	"context"
	"strings"

	"github.com/go-vcs/gitproto/pktline"
)

// LsRefsArgs are the parsed arguments of an ls-refs request
// (gitprotocol-v2, "ls-refs").
type LsRefsArgs struct {
	Symrefs  bool
	Peel     bool
	Unborn   bool
	Prefixes []string
}

// ParseLsRefs parses the argument lines of an ls-refs request. Unknown
// arguments are refused.
func ParseLsRefs(args []string, lim Limits) (LsRefsArgs, error) {
	lim = lim.withDefaults()
	var a LsRefsArgs
	for _, s := range args {
		switch {
		case s == "symrefs":
			a.Symrefs = true
		case s == "peel":
			a.Peel = true
		case s == "unborn":
			a.Unborn = true
		case strings.HasPrefix(s, "ref-prefix "):
			if len(a.Prefixes) >= lim.MaxRefPrefixes {
				return a, protoErr("more than %d ref-prefix lines", lim.MaxRefPrefixes)
			}
			a.Prefixes = append(a.Prefixes, s[len("ref-prefix "):])
		default:
			return a, protoErr("unsupported ls-refs argument %q", clip(s))
		}
	}
	return a, nil
}

func (a LsRefsArgs) match(name string) bool {
	if len(a.Prefixes) == 0 {
		return true
	}
	for _, p := range a.Prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func serveLsRefs(ctx context.Context, w *pktline.Writer, repo Repository, args []string, lim Limits) error {
	a, err := ParseLsRefs(args, lim)
	if err != nil {
		return err
	}
	refs, err := repo.Refs(ctx)
	if err != nil {
		return err
	}
	for _, r := range refs {
		if !a.match(r.Name) {
			continue
		}
		var line strings.Builder
		if r.ID == "" {
			// Unborn HEAD: sent only when the client asked for both
			// "unborn" and "symrefs", as git upload-pack does (the line
			// is useless without the target).
			if !a.Unborn || !a.Symrefs || r.Name != "HEAD" || r.Target == "" {
				continue
			}
			line.WriteString("unborn HEAD symref-target:" + r.Target)
		} else {
			line.WriteString(r.ID + " " + r.Name)
			if a.Symrefs && r.Target != "" {
				line.WriteString(" symref-target:" + r.Target)
			}
			if a.Peel && r.Peeled != "" {
				line.WriteString(" peeled:" + r.Peeled)
			}
		}
		if err := w.Text(line.String()); err != nil {
			return err
		}
	}
	return w.Flush()
}
