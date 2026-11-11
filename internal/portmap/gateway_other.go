//go:build !linux && !windows

package portmap

import (
	"errors"
	"net/netip"
)

// gateway is not implemented here: only UPnP (found by multicast) works.
func gateway() (netip.Addr, error) {
	return netip.Addr{}, errors.New("default gateway lookup not supported")
}
