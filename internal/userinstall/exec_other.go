//go:build !linux

package userinstall

// Handover: there is a per-user copy only on Linux.
func Handover(build int64) error { return ErrNoCopy }
