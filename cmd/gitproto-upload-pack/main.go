// Command gitproto-upload-pack serves git-upload-pack over stdio using
// gitproto: protocol v2 only, fetch side. It is what `git clone
// --upload-pack=<this binary>` runs, with GIT_PROTOCOL=version=2.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/go-vcs/gitproto"
	"github.com/spf13/cobra"
)

var osExit = os.Exit

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	cmd := RootCmd()
	cmd.SetContext(ctx)
	if err := cmd.Execute(); err != nil {
		osExit(1)
	}
}

// RootCmd builds the command. Tests drive it with SetArgs, SetIn, SetOut and
// SetErr.
func RootCmd() *cobra.Command {
	var (
		statelessRPC  bool
		advertiseRefs bool
		threads       int
		gitPath       string
	)
	cmd := &cobra.Command{
		Use:           "gitproto-upload-pack [flags] <directory>",
		Short:         "serve git-upload-pack over stdio (protocol v2, fetch only)",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !protocolV2(os.Getenv("GIT_PROTOCOL")) {
				err := errors.New("only protocol version 2 is supported: GIT_PROTOCOL must contain version=2")
				fmt.Fprintln(cmd.ErrOrStderr(), "gitproto-upload-pack:", err)
				return err
			}
			if st, err := os.Stat(args[0]); err != nil || !st.IsDir() {
				err := fmt.Errorf("%s is not a directory", args[0])
				fmt.Fprintln(cmd.ErrOrStderr(), "gitproto-upload-pack:", err)
				return err
			}
			repo := &gitproto.GitRepo{Dir: gitDir(args[0]), Git: gitPath, PackThreads: threads}
			srv := &gitproto.Server{Repo: repo, Agent: "gitproto"}
			var in io.Reader = cmd.InOrStdin()
			out := cmd.OutOrStdout()
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			var err error
			switch {
			case advertiseRefs:
				err = srv.Advertise(out)
			case statelessRPC:
				err = srv.ServeRequest(ctx, in, out)
				if errors.Is(err, io.EOF) {
					err = nil
				}
			default:
				err = srv.Serve(ctx, in, out)
			}
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "gitproto-upload-pack:", err)
			}
			return err
		},
	}
	f := cmd.Flags()
	f.BoolVar(&statelessRPC, "stateless-rpc", false, "answer one request, no advertisement (smart HTTP)")
	f.BoolVar(&advertiseRefs, "advertise-refs", false, "write the capability advertisement and exit (smart HTTP)")
	f.IntVar(&threads, "pack-threads", 0, "git pack-objects --threads (0: git's default)")
	f.StringVar(&gitPath, "git", "", "git executable (default: git from PATH)")
	return cmd
}

// protocolV2 reports whether a GIT_PROTOCOL value (colon-separated
// key=value pairs) requests version=2.
func protocolV2(v string) bool {
	for _, kv := range strings.Split(v, ":") {
		if kv == "version=2" {
			return true
		}
	}
	return false
}

// gitDir returns dir/.git when dir is a work tree, dir otherwise.
func gitDir(dir string) string {
	if st, err := os.Stat(dir + string(os.PathSeparator) + ".git"); err == nil && st.IsDir() {
		return dir + string(os.PathSeparator) + ".git"
	}
	return dir
}
