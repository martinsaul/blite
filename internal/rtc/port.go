package rtc

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"blite/internal/portmap"
)

type portMapper interface {
	State() portmap.State
	Close()
}

// Port is our one UDP port for all peer connections, optionally mapped on the
// router while we run. Its socket is open while a mesh uses it, so it binds to
// the interfaces of the moment.
type Port struct {
	pref        int
	state       func() portmap.State // tests replace it
	startMapper func(port int, log *slog.Logger, onChange func(portmap.State)) portMapper
	log         *slog.Logger

	mu      sync.Mutex
	mapper  portMapper
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
	return NewPortOpts(pref, true, log)
}

// NewPortOpts picks the first free port from pref on and, when mapRouter is
// true, maps it on the router via PCP/NAT-PMP/UPnP. When mapRouter is false,
// no router mapping requests are sent, while peer connections still share the
// single fixed local UDP port.
func NewPortOpts(pref int, mapRouter bool, log *slog.Logger) *Port {
	if log == nil {
		log = slog.Default()
	}
	p := &Port{
		pref: freePort(pref),
		log:  log,
		startMapper: func(port int, log *slog.Logger, onChange func(portmap.State)) portMapper {
			return portmap.Start(port, log, onChange)
		},
	}
	if mapRouter {
		p.SetMapping(true)
	}
	return p
}

func (p *Port) notifyWatches() {
	p.mu.Lock()
	ws := append([]func(){}, p.watches...)
	p.mu.Unlock()
	for _, f := range ws {
		f()
	}
}

// SetMapping turns router port mapping (PCP/NAT-PMP/UPnP) on or off while
// keeping the fixed local UDP port.
func (p *Port) SetMapping(on bool) {
	p.mu.Lock()
	if on {
		if p.mapper != nil {
			p.mu.Unlock()
			return
		}
		start := p.startMapper
		if start == nil {
			start = func(port int, log *slog.Logger, onChange func(portmap.State)) portMapper {
				return portmap.Start(port, log, onChange)
			}
		}
		p.mapper = start(p.pref, p.log, func(portmap.State) {
			p.notifyWatches()
		})
		p.mu.Unlock()
		p.notifyWatches()
		return
	}
	m := p.mapper
	p.mapper = nil
	p.mu.Unlock()
	if m != nil {
		m.Close()
		p.notifyWatches()
	}
}

// Mapping reports whether router port mapping is active on this port.
func (p *Port) Mapping() bool {
	if p.state != nil {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mapper != nil
}

// Watch calls f whenever the router mapping changes.
func (p *Port) Watch(f func()) {
	p.mu.Lock()
	p.watches = append(p.watches, f)
	p.mu.Unlock()
}

// State is what the router mapping looks like.
func (p *Port) State() portmap.State {
	if p.state != nil {
		return p.state()
	}
	p.mu.Lock()
	m := p.mapper
	p.mu.Unlock()
	if m == nil {
		return portmap.State{}
	}
	return m.State()
}

// Close removes any router mapping.
func (p *Port) Close() { p.SetMapping(false) }

// usableIP skips link-local addresses: useless for calls, and a container
// interface's (host networking) can't be bound for a moment after it
// appears, which failed the whole port.
func usableIP(ip net.IP) bool { return !ip.IsLinkLocalUnicast() }

// virtualPrefixes name container and VM bridges: Docker, libvirt, LXC,
// Podman and Hyper-V/WSL. A peer can't reach us through them, so offering
// them only hands out internal addresses (host networking sees them all).
var virtualPrefixes = []string{"docker", "br-", "veth", "virbr", "lxc", "lxdbr", "cni", "podman", "vethernet"}

// usableInterface skips container and VM bridges (see virtualPrefixes).
func usableInterface(name string) bool {
	name = strings.ToLower(name)
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

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
			if p.mux, err = ice.NewMultiUDPMuxFromPort(port, ice.UDPMuxFromPortWithIPFilter(usableIP), ice.UDPMuxFromPortWithInterfaceFilter(usableInterface)); err == nil {
				p.port = port
				break
			}
			p.log.Info("udp port unusable", "port", port, "err", err)
		}
		if p.mux == nil {
			return nil, 0, fmt.Errorf("no free UDP port in %d-%d: %w", p.pref, p.pref+portTries-1, err)
		}
		if p.port != p.pref && p.mapper != nil {
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
