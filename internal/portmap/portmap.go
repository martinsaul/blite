// Package portmap opens our UDP port on the home router, so that
// peers behind strict NATs can reach us: PCP, NAT-PMP or UPnP IGD, whichever
// the router speaks. The mapping is renewed while we run and removed on
// Close.
package portmap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"
)

// DefaultPort is the UDP port a client uses for all peer connections:
// below the Windows and Linux ephemeral ranges and not in /etc/services.
const DefaultPort = 30471

const (
	lease       = 2 * time.Hour
	maxRenew    = 20 * time.Minute // also the refresh of permanent UPnP leases
	retryEvery  = 5 * time.Minute  // after no gateway mapped the port
	pmpTimeout  = 1500 * time.Millisecond
	upnpTimeout = 3 * time.Second
)

// State is what we know about our reachability.
type State struct {
	// Port is our local UDP port.
	Port int
	// Trying: the first attempt is still running.
	Trying bool
	// Public: our own address is public, no NAT in the way.
	Public bool
	// Method is how the port was mapped (UPnP, NAT-PMP, PCP), "" if not.
	Method string
	// External is the mapped public address, Local our address it forwards
	// to.
	External netip.AddrPort
	Local    netip.Addr
	// Err says why there is no usable mapping.
	Err string
}

// Usable reports whether peers can reach External.
func (s State) Usable() bool { return s.Method != "" && s.Err == "" && s.External.IsValid() }

type mapper interface {
	name() string
	local() netip.Addr
	mapPort(ctx context.Context, internal uint16, life time.Duration) (netip.AddrPort, time.Duration, error)
	unmap(ctx context.Context, internal uint16) error
}

// Mapper keeps one port mapped.
type Mapper struct {
	port     uint16
	log      *slog.Logger
	onChange func(State)
	// find returns the router's mappers in the order to try (tests replace it).
	find   func(ctx context.Context) []func(context.Context) (mapper, error)
	cancel context.CancelFunc
	done   chan struct{}

	mu    sync.Mutex
	state State
	cur   mapper
}

// Start maps port in the background. onChange (may be nil) is told every
// change of State.
func Start(port int, log *slog.Logger, onChange func(State)) *Mapper {
	m := newMapper(port, log, onChange)
	m.find = findMappers
	m.run()
	return m
}

func newMapper(port int, log *slog.Logger, onChange func(State)) *Mapper {
	if log == nil {
		log = slog.Default()
	}
	return &Mapper{port: uint16(port), log: log, onChange: onChange, done: make(chan struct{}),
		state: State{Port: port, Trying: true}}
}

func (m *Mapper) run() {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	go m.loop(ctx)
}

// State is the current state.
func (m *Mapper) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Close stops renewing and removes the mapping.
func (m *Mapper) Close() {
	m.cancel()
	<-m.done
	m.mu.Lock()
	cur := m.cur
	m.cur = nil
	m.mu.Unlock()
	if cur != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := cur.unmap(ctx, m.port); err != nil {
			m.log.Info("portmap: unmap failed", "method", cur.name(), "err", err)
		}
	}
}

func (m *Mapper) set(s State) {
	s.Port = int(m.port)
	m.mu.Lock()
	changed := s != m.state
	m.state = s
	m.mu.Unlock()
	if changed {
		m.log.Info("portmap", "port", s.Port, "method", s.Method, "external", s.External, "public", s.Public, "err", s.Err)
		if m.onChange != nil {
			m.onChange(s)
		}
	}
}

func (m *Mapper) loop(ctx context.Context) {
	defer close(m.done)
	for {
		wait := retryEvery
		if mp, ext, life, err := m.open(ctx); err == nil {
			m.mu.Lock()
			m.cur = mp
			m.mu.Unlock()
			s := State{Method: mp.name(), External: ext, Local: mp.local()}
			if !publicAddr(ext.Addr()) {
				s.Err = "the router's own address " + ext.Addr().String() + " is not public (another NAT in front of it)"
			}
			m.set(s)
			wait = m.renewLoop(ctx, mp, life)
		} else if ctx.Err() == nil {
			s := State{Err: err.Error()}
			if ip, err := localIPTo(netip.MustParseAddr("192.0.2.1")); err == nil && publicAddr(ip) {
				s = State{Public: true}
			}
			m.set(s)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// renewLoop renews mp until it fails or ctx ends; it returns how long to
// wait before mapping afresh.
func (m *Mapper) renewLoop(ctx context.Context, mp mapper, life time.Duration) time.Duration {
	for {
		every := life / 2
		if life <= 0 || every > maxRenew {
			every = maxRenew
		}
		every = max(every, 30*time.Second)
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(every):
		}
		rctx, cancel := context.WithTimeout(ctx, upnpTimeout)
		ext, got, err := mp.mapPort(rctx, m.port, lease)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return 0
			}
			m.log.Info("portmap: renew failed", "method", mp.name(), "err", err)
			m.mu.Lock()
			m.cur = nil
			m.mu.Unlock()
			m.set(State{Err: "renewing the mapping failed: " + err.Error()})
			return 30 * time.Second
		}
		life = got
		s := m.State()
		s.External = ext
		m.set(s)
	}
}

// open tries each way the router may speak, in order.
func (m *Mapper) open(ctx context.Context) (mapper, netip.AddrPort, time.Duration, error) {
	var errs []error
	for _, try := range m.find(ctx) {
		if ctx.Err() != nil {
			return nil, netip.AddrPort{}, 0, ctx.Err()
		}
		mp, err := try(ctx)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		timeout := upnpTimeout
		if _, ok := mp.(*pmp); ok {
			timeout = pmpTimeout
		}
		tctx, cancel := context.WithTimeout(ctx, timeout)
		ext, life, err := mp.mapPort(tctx, m.port, lease)
		cancel()
		if err == nil {
			return mp, ext, life, nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		errs = append(errs, errors.New("no router found"))
	}
	return nil, netip.AddrPort{}, 0, errors.Join(errs...)
}

// pmp speaks PCP to the gateway, or NAT-PMP once the gateway said it
// doesn't know PCP.
type pmp struct {
	pcp    *pcp
	natpmp natpmp
	old    bool // NAT-PMP
}

func (p *pmp) name() string {
	if p.old {
		return p.natpmp.name()
	}
	return p.pcp.name()
}

func (p *pmp) local() netip.Addr { return p.pcp.local }

func (p *pmp) mapPort(ctx context.Context, internal uint16, life time.Duration) (netip.AddrPort, time.Duration, error) {
	if !p.old {
		ext, got, err := p.pcp.mapPort(ctx, internal, life)
		if !errors.Is(err, errUnsupportedVersion) {
			return ext, got, err
		}
		p.old = true
	}
	return p.natpmp.mapPort(ctx, internal, life)
}

func (p *pmp) unmap(ctx context.Context, internal uint16) error {
	if p.old {
		return p.natpmp.unmap(ctx, internal)
	}
	return p.pcp.unmap(ctx, internal)
}

// findMappers lists PCP and NAT-PMP at the default gateway, then UPnP.
func findMappers(ctx context.Context) []func(context.Context) (mapper, error) {
	var out []func(context.Context) (mapper, error)
	if gw, err := gateway(); err == nil {
		gwp := netip.AddrPortFrom(gw, pmpPort)
		out = append(out, func(context.Context) (mapper, error) {
			local, err := localIPTo(gw)
			if err != nil {
				return nil, err
			}
			return &pmp{pcp: newPCP(gwp, local), natpmp: natpmp{gw: gwp}}, nil
		})
	}
	out = append(out, func(ctx context.Context) (mapper, error) {
		local, err := localIPTo(netip.MustParseAddr("192.0.2.1"))
		if err != nil {
			return nil, err
		}
		dctx, cancel := context.WithTimeout(ctx, upnpTimeout)
		defer cancel()
		locs, err := discoverIGD(dctx, local)
		if err != nil {
			return nil, err
		}
		var last error
		for _, loc := range locs {
			u, err := newUPnP(ctx, loc)
			if err == nil {
				return u, nil
			}
			last = err
		}
		return nil, last
	})
	return out
}

// publicAddr reports whether ip is a public unicast address (not private,
// CGNAT or link-local).
func publicAddr(ip netip.Addr) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	return !netip.MustParsePrefix("100.64.0.0/10").Contains(ip)
}

// Describe is a line for the user: whether others can reach us, and
// whether that's fine.
func (s State) Describe() (ok bool, text string) {
	const strict = " Calls with people behind strict routers may fail."
	switch {
	case s.Trying:
		return true, "Checking whether others can reach you..."
	case s.Usable():
		return true, fmt.Sprintf("Reachable from the internet on port %d (%s).", s.External.Port(), s.Method)
	case s.Public:
		return true, "Your computer has a public address: others can reach it directly unless a firewall blocks them."
	case s.Method != "":
		return false, "Not reachable: your router opened the port, but it sits behind another router or your provider's NAT." + strict
	}
	return false, "Not reachable: your router doesn't allow port mapping (UPnP, NAT-PMP or PCP)." + strict
}
