package portmap

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// NAT-PMP (RFC 6886) and PCP (RFC 6887) both talk to the gateway on UDP
// 5351. PCP is tried first; a NAT-PMP-only gateway answers it with
// "unsupported version".
const pmpPort = 5351

var errUnsupportedVersion = errors.New("unsupported protocol version")

// exchange sends req to gw and returns the first reply that check accepts,
// resending with backoff (250 ms, doubling) until ctx ends.
func exchange(ctx context.Context, gw netip.AddrPort, req []byte, check func([]byte) bool) ([]byte, error) {
	c, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(gw))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	buf := make([]byte, 1100)
	wait := 250 * time.Millisecond
	for {
		if _, err := c.Write(req); err != nil {
			return nil, err
		}
		dl := time.Now().Add(wait)
		if d, ok := ctx.Deadline(); ok && d.Before(dl) {
			dl = d
		}
		c.SetReadDeadline(dl)
		for {
			n, err := c.Read(buf)
			if err != nil {
				break
			}
			if check(buf[:n]) {
				return buf[:n], nil
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		wait *= 2
	}
}

// localIPTo is our address on the route to dst.
func localIPTo(dst netip.Addr) (netip.Addr, error) {
	c, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(dst, 9)))
	if err != nil {
		return netip.Addr{}, err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap(), nil
}

// natpmp maps through a NAT-PMP gateway.
type natpmp struct{ gw netip.AddrPort }

func (natpmp) name() string { return "NAT-PMP" }

func (n natpmp) externalIP(ctx context.Context) (netip.Addr, error) {
	resp, err := exchange(ctx, n.gw, []byte{0, 0}, func(b []byte) bool { return len(b) >= 12 && b[0] == 0 && b[1] == 128 })
	if err != nil {
		return netip.Addr{}, err
	}
	if rc := binary.BigEndian.Uint16(resp[2:]); rc != 0 {
		return netip.Addr{}, fmt.Errorf("NAT-PMP result %d", rc)
	}
	return netip.AddrFrom4([4]byte(resp[8:12])), nil
}

func (n natpmp) mapPort(ctx context.Context, internal uint16, life time.Duration) (netip.AddrPort, time.Duration, error) {
	ip, err := n.externalIP(ctx)
	if err != nil {
		return netip.AddrPort{}, 0, err
	}
	ext, got, err := n.request(ctx, internal, internal, life)
	if err != nil {
		return netip.AddrPort{}, 0, err
	}
	return netip.AddrPortFrom(ip, ext), got, nil
}

func (n natpmp) unmap(ctx context.Context, internal uint16) error {
	_, _, err := n.request(ctx, internal, 0, 0)
	return err
}

func (n natpmp) request(ctx context.Context, internal, ext uint16, life time.Duration) (uint16, time.Duration, error) {
	req := make([]byte, 12)
	req[1] = 1 // map UDP
	binary.BigEndian.PutUint16(req[4:], internal)
	binary.BigEndian.PutUint16(req[6:], ext)
	binary.BigEndian.PutUint32(req[8:], uint32(life/time.Second))
	resp, err := exchange(ctx, n.gw, req, func(b []byte) bool {
		return len(b) >= 16 && b[0] == 0 && b[1] == 129 && binary.BigEndian.Uint16(b[8:]) == internal
	})
	if err != nil {
		return 0, 0, err
	}
	if rc := binary.BigEndian.Uint16(resp[2:]); rc != 0 {
		return 0, 0, fmt.Errorf("NAT-PMP result %d", rc)
	}
	return binary.BigEndian.Uint16(resp[10:]), time.Duration(binary.BigEndian.Uint32(resp[12:])) * time.Second, nil
}

// pcp maps through a PCP gateway (MAP opcode).
type pcp struct {
	gw    netip.AddrPort
	local netip.Addr
	nonce [12]byte
}

func newPCP(gw netip.AddrPort, local netip.Addr) *pcp {
	p := &pcp{gw: gw, local: local}
	rand.Read(p.nonce[:])
	return p
}

func (*pcp) name() string { return "PCP" }

func (p *pcp) mapPort(ctx context.Context, internal uint16, life time.Duration) (netip.AddrPort, time.Duration, error) {
	return p.request(ctx, internal, internal, life)
}

func (p *pcp) unmap(ctx context.Context, internal uint16) error {
	_, _, err := p.request(ctx, internal, 0, 0)
	return err
}

func (p *pcp) request(ctx context.Context, internal, ext uint16, life time.Duration) (netip.AddrPort, time.Duration, error) {
	req := make([]byte, 60)
	req[0], req[1] = 2, 1
	binary.BigEndian.PutUint32(req[4:], uint32(life/time.Second))
	a16 := p.local.As16() // IPv4 as v4-mapped
	copy(req[8:24], a16[:])
	copy(req[24:36], p.nonce[:])
	req[36] = 17 // UDP
	binary.BigEndian.PutUint16(req[40:], internal)
	binary.BigEndian.PutUint16(req[42:], ext)
	any4 := netip.AddrFrom4([4]byte{}).As16()
	copy(req[44:60], any4[:])
	resp, err := exchange(ctx, p.gw, req, func(b []byte) bool {
		if len(b) >= 4 && b[0] == 0 { // a NAT-PMP gateway: unsupported version
			return true
		}
		return len(b) >= 60 && b[0] == 2 && b[1] == 0x81 && [12]byte(b[24:36]) == p.nonce
	})
	if err != nil {
		return netip.AddrPort{}, 0, err
	}
	if resp[0] == 0 {
		return netip.AddrPort{}, 0, errUnsupportedVersion
	}
	if rc := resp[3]; rc != 0 {
		return netip.AddrPort{}, 0, fmt.Errorf("PCP result %d", rc)
	}
	ip := netip.AddrFrom16([16]byte(resp[44:60])).Unmap()
	return netip.AddrPortFrom(ip, binary.BigEndian.Uint16(resp[42:])), time.Duration(binary.BigEndian.Uint32(resp[4:])) * time.Second, nil
}
