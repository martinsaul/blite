//go:build !windows

package secret

import "slices"

func protect(b []byte) ([]byte, error) {
	return slices.Clone(b), nil
}

func unprotect(b []byte) ([]byte, error) {
	return slices.Clone(b), nil
}
