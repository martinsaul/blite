package portmap

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// gateway is the IPv4 default gateway, from /proc/net/route.
func gateway() (netip.Addr, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return netip.Addr{}, err
	}
	defer f.Close()
	return parseRoutes(bufio.NewScanner(f))
}

// parseRoutes finds the default route with the lowest metric.
func parseRoutes(sc *bufio.Scanner) (netip.Addr, error) {
	var best netip.Addr
	bestMetric := -1
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		b, err := hex.DecodeString(f[2])
		if err != nil || len(b) != 4 {
			continue
		}
		var a [4]byte
		binary.LittleEndian.PutUint32(a[:], binary.BigEndian.Uint32(b))
		ip := netip.AddrFrom4(a)
		metric, err := strconv.Atoi(f[6])
		if err != nil || ip.IsUnspecified() {
			continue
		}
		if bestMetric < 0 || metric < bestMetric {
			best, bestMetric = ip, metric
		}
	}
	if bestMetric < 0 {
		return netip.Addr{}, errors.New("no default route")
	}
	return best, nil
}
