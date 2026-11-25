package gitproto

import (
	"context"
	"io"
	"strings"

	"github.com/go-vcs/gitproto/pktline"
)

// FetchArgs are the parsed arguments of a fetch request.
type FetchArgs struct {
	Wants      []string
	Haves      []string
	Done       bool
	ThinPack   bool
	NoProgress bool
	IncludeTag bool
	OfsDelta   bool
}

// unsupportedFetch lists the fetch arguments of gitprotocol-v2 that belong to
// features this server does not advertise (shallow, filter, ref-in-want,
// sideband-all, packfile-uris, wait-for-done). They are refused by name.
var unsupportedFetch = []string{
	"shallow", "deepen", "deepen-relative", "deepen-since", "deepen-not",
	"filter", "want-ref", "sideband-all", "packfile-uris", "wait-for-done",
}

// ParseFetch parses the argument lines of a fetch request. Arguments of a
// feature the server does not advertise, and unknown arguments, are refused;
// so is an object id that is not 40 lowercase hex digits.
func ParseFetch(args []string, lim Limits) (FetchArgs, error) {
	lim = lim.withDefaults()
	var a FetchArgs
	seenWant := map[string]bool{}
	seenHave := map[string]bool{}
	for _, s := range args {
		key, val, hasVal := strings.Cut(s, " ")
		switch key {
		case "want", "have":
			if !hasVal || !ValidOID(val) {
				return a, protoErr("%s: invalid object id %q", key, clip(val))
			}
			if key == "want" {
				if seenWant[val] {
					continue
				}
				if len(a.Wants) >= lim.MaxWants {
					return a, protoErr("more than %d want lines", lim.MaxWants)
				}
				seenWant[val] = true
				a.Wants = append(a.Wants, val)
			} else {
				if seenHave[val] {
					continue
				}
				if len(a.Haves) >= lim.MaxHaves {
					return a, protoErr("more than %d have lines", lim.MaxHaves)
				}
				seenHave[val] = true
				a.Haves = append(a.Haves, val)
			}
		case "done", "thin-pack", "no-progress", "include-tag", "ofs-delta":
			if hasVal {
				return a, protoErr("%s takes no value", key)
			}
			switch key {
			case "done":
				a.Done = true
			case "thin-pack":
				a.ThinPack = true
			case "no-progress":
				a.NoProgress = true
			case "include-tag":
				a.IncludeTag = true
			case "ofs-delta":
				a.OfsDelta = true
			}
		default:
			for _, u := range unsupportedFetch {
				if key == u {
					return a, protoErr("unsupported fetch argument %q: the %q feature is not advertised", u, u)
				}
			}
			return a, protoErr("unknown fetch argument %q", clip(key))
		}
	}
	return a, nil
}

func serveFetch(ctx context.Context, w *pktline.Writer, repo Repository, args []string, lim Limits) error {
	a, err := ParseFetch(args, lim)
	if err != nil {
		return err
	}
	if len(a.Wants) == 0 {
		// Nothing was asked for: an empty response.
		return w.Flush()
	}
	// A want must be an advertised tip (or the peeled object of one): the
	// server never serves an object only because the client can name it.
	// Usually every want is a ref's own object, which needs no peeling (and
	// peeling can cost a process), so that is checked first when the
	// repository can list refs without it.
	if err := checkWants(ctx, repo, a.Wants); err != nil {
		return err
	}

	common, err := repo.CommonCommits(ctx, a.Haves)
	if err != nil {
		return err
	}

	if !a.Done {
		// Write errors are sticky on w: they surface at the Flush below or
		// at the Err check before the pack is generated.
		_ = w.Text("acknowledgments")
		if len(common) == 0 {
			_ = w.Text("NAK")
			return w.Flush()
		}
		for _, id := range common {
			_ = w.Text("ACK " + id)
		}
		_ = w.Text("ready")
		_ = w.Delim()
	}

	_ = w.Text("packfile")
	if err := w.Err(); err != nil {
		return err // never generate a pack nobody can receive
	}
	sb := &sidebandWriter{w: w}
	err = repo.WritePack(ctx, PackRequest{
		Wants:      a.Wants,
		Common:     common,
		Thin:       a.ThinPack,
		OfsDelta:   a.OfsDelta,
		IncludeTag: a.IncludeTag,
	}, sb)
	if err != nil {
		// Side-band 3: fatal error just before the stream aborts. The
		// message is generic; the cause is returned to the caller only.
		_ = w.Sideband(3, []byte("ERR pack generation failed\n"))
		return err
	}
	return w.Flush()
}

// notTip returns the first want that is neither a ref's object nor, when
// peeled is set, a ref's peeled object; "" when every want is a tip.
func notTip(wants []string, refs []Ref, peeled bool) string {
	tips := make(map[string]bool, 2*len(refs))
	for _, r := range refs {
		if r.ID != "" {
			tips[r.ID] = true
		}
		if peeled && r.Peeled != "" {
			tips[r.Peeled] = true
		}
	}
	for _, w := range wants {
		if !tips[w] {
			return w
		}
	}
	return ""
}

func checkWants(ctx context.Context, repo Repository, wants []string) error {
	if quick, ok := repo.(interface {
		RefsUnpeeled(context.Context) ([]Ref, error)
	}); ok {
		refs, err := quick.RefsUnpeeled(ctx)
		if err != nil {
			return err
		}
		if notTip(wants, refs, false) == "" {
			return nil
		}
	}
	refs, err := repo.Refs(ctx)
	if err != nil {
		return err
	}
	if w := notTip(wants, refs, true); w != "" {
		return protoErr("not our ref %s", w)
	}
	return nil
}

// sidebandWriter frames pack bytes on side-band channel 1.
type sidebandWriter struct{ w *pktline.Writer }

func (s *sidebandWriter) Write(p []byte) (int, error) {
	if err := s.w.Sideband(1, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

var _ io.Writer = (*sidebandWriter)(nil)
