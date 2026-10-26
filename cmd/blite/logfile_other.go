//go:build !linux && !windows

package main

import (
	"errors"
	"os"
)

func logDir() (string, error)         { return "", errors.New("no log files on this OS") }
func keepStderr() bool                { return true }
func redirectStderr(f *os.File) error { return errors.New("unsupported") }
