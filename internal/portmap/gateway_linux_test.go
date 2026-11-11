package portmap

import (
	"bufio"
	"net/netip"
	"strings"
	"testing"
)

func TestParseRoutes(t *testing.T) {
	const tab = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"wg0\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"wlan0\t00000000\t0101A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
		"eth0\t00000000\t0100000A\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"eth0\t0000000A\t00000000\t0001\t0\t0\t100\t0000E0FF\t0\t0\t0\n"
	gw, err := parseRoutes(bufio.NewScanner(strings.NewReader(tab)))
	if err != nil || gw != netip.MustParseAddr("10.0.0.1") {
		t.Fatalf("gateway = %v, %v; want 10.0.0.1", gw, err)
	}
}
