package rtc

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"blite/internal/portmap"
)

// Port is our one UDP port for all peer connections, mapped on
// the router while we run. Its socket is open while a mesh uses it, so it
// binds to the interfaces of the moment.
type Port struct {
	pref   int
	mapper *portmap.Mapper
	state  func() portmap.State // mapper.State; tests replace it
	log    *slog.Logger

	mu      sync.Mutex
	mux     *ice.MultiUDPMuxDefault
	port    int
	refs    int
	watches []func()
}

// portTries is how many ports after the preferred one we try when it is
// busy (e.g. a second client on the same machine).
const portTries = 10

// NewPort picks the first free port from pref on, maps it on the router and
// keeps it mapped until Close.
func NewPort(pref int, log *slog.Logger) *Port {
	if log == nil {
		log = slog.Default()
	}
	p := &Port{pref: freePort(pref), log: log}
	p.mapper = portmap.Start(p.pref, log, func(portmap.State) {
		p.mu.Lock()
		ws := p.watches
		p.mu.Unlock()
		for _, f := range ws {
			f()
		}
	})
	p.state = p.mapper.State
	return p
}

// Watch calls f whenever the router mapping changes.
func (p *Port) Watch(f func()) {
	p.mu.Lock()
	p.watches = append(p.watches, f)
	p.mu.Unlock()
}

// State is what the router mapping looks like.
func (p *Port) State() portmap.State { return p.state() }

// Close removes the router mapping.
func (p *Port) Close() { p.mapper.Close() }

// usableIP skips link-local addresses: useless for calls, and a container
// interface's (host networking) can't be bound for a moment after it
// appears, which failed the whole port.
func usableIP(ip net.IP) bool { return !ip.IsLinkLocalUnicast() }

// freePort is the first port from pref on that nobody has bound (another
// instance on this machine must not map, and on exit unmap, ours).
func freePort(pref int) int {
	for port := pref; port < pref+portTries; port++ {
		c, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
		if err == nil {
			c.Close()
			return port
		}
	}
	return pref
}

// acquire opens (or shares) the socket and returns it with its port.
func (p *Port) acquire() (ice.UDPMux, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mux == nil {
		var err error
		for port := p.pref; port < p.pref+portTries; port++ {
			if p.mux, err = ice.NewMultiUDPMuxFromPort(port, ice.UDPMuxFromPortWithIPFilter(usableIP)); err == nil {
				p.port = port
				break
			}
			p.log.Info("udp port unusable", "port", port, "err", err)
		}
		if p.mux == nil {
			return nil, 0, fmt.Errorf("no free UDP port in %d-%d: %w", p.pref, p.pref+portTries-1, err)
		}
		if p.port != p.pref {
			p.log.Warn("udp port differs from the mapped one: peers can't use the router mapping", "port", p.port, "mapped", p.pref)
		}
	}
	p.refs++
	return p.mux, p.port, nil
}

// recycle closes the shared socket and opens it again, if we
// are its only user: a long-lived socket once kept a rejoined peer from
// connecting until the app restarted. It returns the new mux, or nil if
// it left the socket alone.
func (p *Port) recycle() ice.UDPMux {
	p.mu.Lock()
	if p.refs != 1 || p.mux == nil {
		p.mu.Unlock()
		return nil
	}
	p.mux.Close()
	p.mux, p.refs = nil, 0
	p.mu.Unlock()
	mux, _, err := p.acquire()
	if err != nil {
		p.log.Warn("reopen udp port", "err", err)
		return nil
	}
	return mux
}

func (p *Port) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refs--; p.refs == 0 {
		p.mux.Close()
		p.mux = nil
	}
}

// mappedCandidate is the candidate to add next to c, our host candidate on
// the port: the router's public address that forwards to it; nil if there
// is none.
func (p *Port) mappedCandidate(c *webrtc.ICECandidate, port int) *webrtc.ICECandidateInit {
	st := p.State()
	if !st.Usable() || port != p.pref || c.Typ != webrtc.ICECandidateTypeHost || c.Protocol != webrtc.ICEProtocolUDP ||
		int(c.Port) != port || c.Address != st.Local.String() {
		return nil
	}
	ci := c.ToJSON()
	// Server reflexive priority: type preference 100, as RFC 8445 suggests.
	prio := uint32(100)<<24 | uint32(65535)<<8 | 255
	ci.Candidate = "candidate:" + strconv.Itoa(int(st.External.Port())) + "1 1 udp " + strconv.FormatUint(uint64(prio), 10) + " " +
		st.External.Addr().String() + " " + strconv.Itoa(int(st.External.Port())) + " typ srflx raddr " +
		c.Address + " rport " + strconv.Itoa(port)
	return &ci
}
