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
