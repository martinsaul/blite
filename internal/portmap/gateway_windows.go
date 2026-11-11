package portmap

import (
	"errors"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/windows"
)

// gateway is the IPv4 default gateway with the lowest metric.
func gateway() (netip.Addr, error) {
	var t *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_INET, &t); err != nil {
		return netip.Addr{}, err
	}
	defer windows.FreeMibTable(unsafe.Pointer(t))
	var best netip.Addr
	var bestMetric uint32
	found := false
	for _, r := range t.Rows() {
		if r.DestinationPrefix.PrefixLength != 0 || r.NextHop.Family != windows.AF_INET {
			continue
		}
		ip := netip.AddrFrom4(*(*[4]byte)(unsafe.Pointer(&r.NextHop.Data[0])))
		if ip.IsUnspecified() {
			continue
		}
		if !found || r.Metric < bestMetric {
			best, bestMetric, found = ip, r.Metric, true
		}
	}
	if !found {
		return netip.Addr{}, errors.New("no default route")
	}
	return best, nil
}
