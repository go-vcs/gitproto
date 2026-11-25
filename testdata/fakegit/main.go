// Command fakegit is a scripted stand-in for git, built by the tests so that
// they run on every platform (no POSIX shell needed). It answers from files
// next to the executable: for the key K it drains stdin, prints K.out when it
// exists and exits with the number in K.rc (0 when absent). K is the first
// argument, except for rev-parse where it is "rev-parse." plus the last
// argument with every character outside [A-Za-z0-9] replaced by "_".
package main

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	dir := filepath.Dir(os.Args[0])
	args := os.Args[1:]
	k := ""
	if len(args) > 0 {
		k = args[0]
	}
	if k == "rev-parse" {
		last := args[len(args)-1]
		var b strings.Builder
		for _, c := range []byte(last) {
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
				b.WriteByte(c)
			} else {
				b.WriteByte('_')
			}
		}
		k = "rev-parse." + b.String()
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	if out, err := os.ReadFile(filepath.Join(dir, k+".out")); err == nil {
		_, _ = os.Stdout.Write(out)
	}
	rc := 0
	if b, err := os.ReadFile(filepath.Join(dir, k+".rc")); err == nil {
		rc, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	os.Exit(rc)
}
