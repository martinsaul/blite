package rtc

import (
	"io"
	"log/slog"
	"net/netip"
	"testing"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"blite/internal/portmap"
)

func TestMappedCandidate(t *testing.T) {
	st := portmap.State{Port: 30471, Method: "UPnP", External: netip.MustParseAddrPort("203.0.113.7:30471"),
		Local: netip.MustParseAddr("192.168.1.20")}
	p := &Port{pref: 30471, state: func() portmap.State { return st }}
	host := &webrtc.ICECandidate{Typ: webrtc.ICECandidateTypeHost, Protocol: webrtc.ICEProtocolUDP,
		Address: "192.168.1.20", Port: 30471, Priority: 2130706431, Foundation: "1", Component: 1}
	mc := p.mappedCandidate(host, 30471)
	if mc == nil {
		t.Fatal("no mapped candidate")
	}
	c, err := ice.UnmarshalCandidate(mc.Candidate)
	if err != nil {
		t.Fatalf("%q: %v", mc.Candidate, err)
	}
	if c.Type() != ice.CandidateTypeServerReflexive || c.Address() != "203.0.113.7" || c.Port() != 30471 ||
		c.RelatedAddress().Address != "192.168.1.20" {
		t.Fatalf("candidate %s", c)
	}

	other := *host
	other.Address = "10.8.0.2" // another interface: the router doesn't forward to it
	if p.mappedCandidate(&other, 30471) != nil {
		t.Fatal("mapped candidate for another interface")
	}
	if p.mappedCandidate(host, 30472) != nil {
		t.Fatal("mapped candidate on a port the router doesn't forward")
	}
	st.Err = "not public"
	if p.mappedCandidate(host, 30471) != nil {
		t.Fatal("mapped candidate behind another NAT")
	}
}

// recycle reopens the socket only for its sole user.
func TestPortRecycle(t *testing.T) {
	p := &Port{pref: freePort(31471), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	mux, _, err := p.acquire()
	if err != nil {
		t.Fatal(err)
	}
	p.acquire() // a second user
	if p.recycle() != nil {
		t.Fatal("recycled a socket two meshes use")
	}
	p.release()
	nm := p.recycle()
	if nm == nil || nm == mux {
		t.Fatalf("recycle: %v (old %v)", nm, mux)
	}
	if p.refs != 1 || p.port != p.pref {
		t.Fatalf("after recycle: refs %d port %d (want %d)", p.refs, p.port, p.pref)
	}
	p.release()
}

type fakePortMapper struct {
	st     portmap.State
	closed int
}

func (f *fakePortMapper) State() portmap.State { return f.st }
func (f *fakePortMapper) Close()               { f.closed++ }

func TestPortMappingToggleKeepsFixedLocalPort(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := NewPortOpts(32471, false, quiet)
	defer p.Close()

	starts := 0
	var last *fakePortMapper
	p.startMapper = func(port int, _ *slog.Logger, _ func(portmap.State)) portMapper {
		starts++
		last = &fakePortMapper{
			st: portmap.State{
				Port:     port,
				Method:   "PCP",
				External: netip.MustParseAddrPort("203.0.113.9:32471"),
				Local:    netip.MustParseAddr("192.168.1.20"),
			},
		}
		return last
	}

	if p.Mapping() {
		t.Fatal("Mapping() should be false when started with mapRouter=false")
	}
	// Even with router mapping off, acquire() still binds the single fixed UDP port.
	mux1, port1, err := p.acquire()
	if err != nil {
		t.Fatal(err)
	}
	mux2, port2, err := p.acquire()
	if err != nil {
		t.Fatal(err)
	}
	if mux1 != mux2 || port1 != p.pref || port2 != p.pref {
		t.Fatalf("expected shared fixed local UDP port %d, got %d and %d", p.pref, port1, port2)
	}
	if starts != 0 {
		t.Fatalf("expected 0 router mapping requests when off, got %d", starts)
	}
	host := &webrtc.ICECandidate{
		Typ: webrtc.ICECandidateTypeHost, Protocol: webrtc.ICEProtocolUDP,
		Address: "192.168.1.20", Port: uint16(port1), Priority: 2130706431, Foundation: "1", Component: 1,
	}
	if p.mappedCandidate(host, port1) != nil {
		t.Fatal("mappedCandidate should be nil when router mapping is off")
	}

	// Enable router mapping, then disable it again; fixed local UDP port stays bound.
	p.SetMapping(true)
	if !p.Mapping() || starts != 1 {
		t.Fatalf("after SetMapping(true): Mapping=%v starts=%d", p.Mapping(), starts)
	}
	p.SetMapping(false)
	if p.Mapping() || last == nil || last.closed != 1 {
		t.Fatalf("after SetMapping(false): Mapping=%v closed=%v", p.Mapping(), last)
	}
	if p.mappedCandidate(host, port1) != nil {
		t.Fatal("mappedCandidate should be nil after SetMapping(false)")
	}
	p.release()
	p.release()
}

func TestUsableInterface(t *testing.T) {
	for name, want := range map[string]bool{
		"eth0": true, "enp3s0": true, "wlan0": true, "Wi-Fi": true, "Ethernet": true, "tailscale0": true,
		"docker0": false, "br-1a2b3c": false, "veth9f8e7d": false, "virbr0": false, "lxcbr0": false,
		"lxdbr0": false, "cni0": false, "podman0": false, "vEthernet (WSL)": false,
	} {
		if got := usableInterface(name); got != want {
			t.Errorf("usableInterface(%q) = %v, want %v", name, got, want)
		}
	}
}
