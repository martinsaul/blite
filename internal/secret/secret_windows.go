//go:build windows

package secret

import (
	"runtime"
	"slices"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	entropy   = []byte("bellion-share-v1")
	emptyByte byte
)

func blob(b []byte) windows.DataBlob {
	if len(b) == 0 {
		return windows.DataBlob{Data: &emptyByte}
	}
	return windows.DataBlob{Size: uint32(len(b)), Data: &b[0]}
}

func freeBlob(d windows.DataBlob) {
	if d.Data != nil {
		clear(unsafe.Slice(d.Data, d.Size))
		_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(d.Data)))
	}
}

func protect(b []byte) ([]byte, error) {
	in, ent := blob(b), blob(entropy)
	var out windows.DataBlob
	err := windows.CryptProtectData(&in, nil, &ent, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	runtime.KeepAlive(b)
	runtime.KeepAlive(entropy)
	if err != nil {
		return nil, err
	}
	defer freeBlob(out)
	if out.Size == 0 {
		return []byte{}, nil
	}
	return slices.Clone(unsafe.Slice(out.Data, out.Size)), nil
}

func unprotect(b []byte) ([]byte, error) {
	in, ent := blob(b), blob(entropy)
	var out windows.DataBlob
	err := windows.CryptUnprotectData(&in, nil, &ent, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)
	runtime.KeepAlive(b)
	runtime.KeepAlive(entropy)
	if err != nil {
		return nil, err
	}
	defer freeBlob(out)
	if out.Size == 0 {
		return []byte{}, nil
	}
	return slices.Clone(unsafe.Slice(out.Data, out.Size)), nil
}
