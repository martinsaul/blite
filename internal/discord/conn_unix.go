//go:build !windows

package discord

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// dial connects to Discord's socket, discord-ipc-0..9: in the runtime dir,
// or below it where the Flatpak and Snap builds (and others) put it.
func dial() (io.ReadWriteCloser, error) {
	var bases []string
	for _, k := range []string{"XDG_RUNTIME_DIR", "TMPDIR", "TMP", "TEMP"} {
		if v := os.Getenv(k); v != "" {
			bases = append(bases, v)
		}
	}
	bases = append(bases, fmt.Sprintf("/run/user/%d", os.Getuid()), "/tmp")
	var paths []string
	seen := map[string]bool{}
	for _, b := range bases {
		for _, pat := range []string{"discord-ipc-[0-9]", "*/discord-ipc-[0-9]", "*/*/discord-ipc-[0-9]", "*/*/*/discord-ipc-[0-9]"} {
			m, _ := filepath.Glob(filepath.Join(b, pat))
			sort.Strings(m)
			for _, p := range m {
				if !seen[p] {
					seen[p] = true
					paths = append(paths, p)
				}
			}
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no discord-ipc socket in %s (XDG_RUNTIME_DIR=%q)", strings.Join(bases, ", "), os.Getenv("XDG_RUNTIME_DIR"))
	}
	var errs []string
	for _, p := range paths {
		c, err := net.Dial("unix", p)
		if err == nil {
			return c, nil
		}
		errs = append(errs, err.Error())
	}
	return nil, errors.New(strings.Join(errs, "; "))
}
