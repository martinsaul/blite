//go:build !windows

package diag

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// processes reads /proc (Linux); elsewhere there is none.
func processes() ([]Proc, error) {
	ds, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []Proc
	for _, d := range ds {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", d.Name(), "comm"))
		if err != nil {
			continue
		}
		out = append(out, Proc{Exe: strings.TrimSpace(string(b)), PID: pid})
	}
	return out, nil
}

func sampleGPU() (func() []EngineUse, error) {
	return nil, errNotHere
}
