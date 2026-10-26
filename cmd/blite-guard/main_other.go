//go:build !windows

// Command blite-guard is Windows-only; see main_windows.go.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "blite-guard runs on Windows only")
	os.Exit(2)
}
