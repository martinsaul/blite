package portmap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// UPnP IGD: find the gateway by SSDP, then add the mapping by SOAP on its
// WANIPConnection (or WANPPPConnection) service.

var ssdpAddr = netip.MustParseAddrPort("239.255.255.250:1900")

var searchTargets = []string{
	"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
	"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
	"urn:schemas-upnp-org:service:WANIPConnection:1",
}

// discoverIGD multicasts M-SEARCH from local and returns the LOCATION URLs
// from gw (or our local subnets if gw is invalid) that answer before ctx ends.
func discoverIGD(ctx context.Context, local, gw netip.Addr, dst netip.AddrPort) ([]string, error) {
	c, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(local, 0)))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	for _, st := range searchTargets {
		msg := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: " + st + "\r\n\r\n"
		if _, err := c.WriteToUDPAddrPort([]byte(msg), dst); err != nil {
			return nil, err
		}
	}
	var nets []netip.Prefix
	if !gw.IsValid() {
		nets = localSubnets()
	}
	// Collect for a short while once the first answer arrived: a router may
	// answer each search target, and other devices answer too.
	var locs []string
	seen := map[string]bool{}
	end, _ := ctx.Deadline()
	buf := make([]byte, 2048)
	for {
		c.SetReadDeadline(end)
		n, from, err := c.ReadFromUDPAddrPort(buf)
		if err != nil {
			break
		}
		src := from.Addr().Unmap()
		if !allowedIGD(src, gw, nets) {
			continue
		}
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf[:n])), nil)
		if err != nil {
			continue
		}
		loc := resp.Header.Get("Location")
		if loc == "" || seen[loc] || !sameHostIP(loc, src) {
			continue
		}
		seen[loc] = true
		locs = append(locs, loc)
		if soon := time.Now().Add(300 * time.Millisecond); soon.Before(end) {
			end = soon
		}
	}
	if len(locs) == 0 {
		return nil, errors.New("no UPnP gateway answered")
	}
	return locs, nil
}

// allowedIGD reports whether src may answer as our IGD: either it is gw, or
// (when gw is unknown) a private/link-local address on one of nets.
func allowedIGD(src, gw netip.Addr, nets []netip.Prefix) bool {
	if gw.IsValid() {
		return src == gw
	}
	if src.IsLoopback() || (!src.IsPrivate() && !src.IsLinkLocalUnicast()) {
		return false
	}
	for _, p := range nets {
		if p.Contains(src) {
			return true
		}
	}
	return false
}

// localSubnets returns the IPv4 non-loopback interface prefixes.
func localSubnets() []netip.Prefix {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Prefix
	for _, a := range addrs {
		in, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(in.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if !ip.Is4() || ip.IsLoopback() {
			continue
		}
		ones, bits := in.Mask.Size()
		if bits != 32 || ones <= 0 {
			continue
		}
		out = append(out, netip.PrefixFrom(ip, ones))
	}
	return out
}

// sameHostIP reports whether rawURL is an http URL whose literal host IP is want.
func sameHostIP(rawURL string, want netip.Addr) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "http" {
		return false
	}
	ip, err := netip.ParseAddr(u.Hostname())
	return err == nil && ip.Unmap() == want
}

// upnp is a WAN connection service found on an IGD.
type upnp struct {
	ctrl    string // control URL
	service string // service type
	lan     netip.Addr
	client  *http.Client
	ext     uint16 // the external port we got (0 = none yet)
}

func (*upnp) name() string { return "UPnP" }

func (u *upnp) local() netip.Addr { return u.lan }

var knownWANIP = []string{
	"urn:schemas-upnp-org:service:WANIPConnection:1",
	"urn:schemas-upnp-org:service:WANIPConnection:2",
}

var knownWANPPP = []string{
	"urn:schemas-upnp-org:service:WANPPPConnection:1",
}

type upnpDevice struct {
	Services []struct {
		ServiceType string `xml:"serviceType"`
		ControlURL  string `xml:"controlURL"`
	} `xml:"serviceList>service"`
	Devices []upnpDevice `xml:"deviceList>device"`
}

// findWAN returns the first matching WAN connection service in the tree.
func (d *upnpDevice) findWAN(allowed []string) (svc, ctrl string) {
	for _, s := range d.Services {
		for _, want := range allowed {
			if s.ServiceType == want {
				return s.ServiceType, s.ControlURL
			}
		}
	}
	for i := range d.Devices {
		if svc, ctrl = d.Devices[i].findWAN(allowed); svc != "" {
			return
		}
	}
	return "", ""
}

// newUPnP reads the device description at loc.
func newUPnP(ctx context.Context, loc string) (*upnp, error) {
	u, err := url.Parse(loc)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("UPnP location %q: unsupported scheme", loc)
	}
	host, err := netip.ParseAddr(u.Hostname())
	if err != nil {
		return nil, fmt.Errorf("UPnP location %q: %w", loc, err)
	}
	host = host.Unmap()
	local, err := localIPTo(host)
	if err != nil {
		return nil, err
	}
	cl := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, "GET", loc, nil)
	if err != nil {
		return nil, err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("UPnP description HTTP %d", resp.StatusCode)
	}
	var root struct {
		URLBase string     `xml:"URLBase"`
		Device  upnpDevice `xml:"device"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&root); err != nil {
		return nil, fmt.Errorf("UPnP description: %w", err)
	}
	svc, ctrl := root.Device.findWAN(knownWANIP)
	if svc == "" {
		svc, ctrl = root.Device.findWAN(knownWANPPP)
	}
	if svc == "" {
		return nil, errors.New("UPnP device has no WAN connection service")
	}
	base := u
	if root.URLBase != "" {
		b, err := url.Parse(root.URLBase)
		if err != nil {
			return nil, fmt.Errorf("UPnP URLBase %q: %w", root.URLBase, err)
		}
		base = u.ResolveReference(b)
	}
	c, err := base.Parse(ctrl)
	if err != nil {
		return nil, err
	}
	if c.Scheme != "http" {
		return nil, fmt.Errorf("UPnP control URL %q: unsupported scheme", c)
	}
	if ctrlIP, err := netip.ParseAddr(c.Hostname()); err != nil || ctrlIP.Unmap() != host {
		return nil, fmt.Errorf("UPnP control URL %q does not match device host %s", c, host)
	}
	return &upnp{ctrl: c.String(), service: svc, lan: local, client: cl}, nil
}

// soapError is a UPnP fault, e.g. 718 ConflictInMappingEntry.
type soapError struct {
	Code int
	Desc string
}

func (e *soapError) Error() string { return fmt.Sprintf("UPnP error %d %s", e.Code, e.Desc) }

// call runs action with args (in order) and returns the response fields.
func (u *upnp) call(ctx context.Context, action string, args [][2]string) (map[string]string, error) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:`)
	xml.EscapeText(&b, []byte(action))
	b.WriteString(` xmlns:u="`)
	xml.EscapeText(&b, []byte(u.service))
	b.WriteString(`">`)
	for _, a := range args {
		b.WriteByte('<')
		xml.EscapeText(&b, []byte(a[0]))
		b.WriteByte('>')
		xml.EscapeText(&b, []byte(a[1]))
		b.WriteString("</")
		xml.EscapeText(&b, []byte(a[0]))
		b.WriteByte('>')
	}
	b.WriteString(`</u:`)
	xml.EscapeText(&b, []byte(action))
	b.WriteString(`></s:Body></s:Envelope>`)
	req, err := http.NewRequestWithContext(ctx, "POST", u.ctrl, strings.NewReader(b.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+u.service+"#"+action+`"`)
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return parseSOAP(io.LimitReader(resp.Body, 1<<20), resp.StatusCode)
}

// parseSOAP collects the leaf elements of a SOAP response body; a fault
// becomes a *soapError.
func parseSOAP(r io.Reader, status int) (map[string]string, error) {
	out := map[string]string{}
	d := xml.NewDecoder(r)
	var name string
	var text strings.Builder
	for {
		t, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("UPnP response: %w", err)
		}
		switch t := t.(type) {
		case xml.StartElement:
			name = t.Name.Local
			text.Reset()
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			if name == t.Name.Local {
				out[name] = strings.TrimSpace(text.String())
			}
			name = ""
		}
	}
	if code, ok := out["errorCode"]; ok {
		n, _ := strconv.Atoi(code)
		return nil, &soapError{Code: n, Desc: out["errorDescription"]}
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("UPnP HTTP %d", status)
	}
	return out, nil
}

func (u *upnp) mapPort(ctx context.Context, internal uint16, life time.Duration) (netip.AddrPort, time.Duration, error) {
	r, err := u.call(ctx, "GetExternalIPAddress", nil)
	if err != nil {
		return netip.AddrPort{}, 0, err
	}
	ip, err := netip.ParseAddr(r["NewExternalIPAddress"])
	if err != nil {
		return netip.AddrPort{}, 0, fmt.Errorf("UPnP external address %q", r["NewExternalIPAddress"])
	}
	add := func(ext uint16, life time.Duration) error {
		_, err := u.call(ctx, "AddPortMapping", [][2]string{
			{"NewRemoteHost", ""},
			{"NewExternalPort", strconv.Itoa(int(ext))},
			{"NewProtocol", "UDP"},
			{"NewInternalPort", strconv.Itoa(int(internal))},
			{"NewInternalClient", u.lan.String()},
			{"NewEnabled", "1"},
			{"NewPortMappingDescription", "Bellion Share"},
			{"NewLeaseDuration", strconv.Itoa(int(life / time.Second))},
		})
		return err
	}
	// Another machine on the LAN may hold the external port (718
	// ConflictInMappingEntry; some routers say 606 Action not authorized
	// instead): try the next ones. A renewal asks for the one we got.
	ext := internal
	if u.ext != 0 {
		ext = u.ext
	}
	for try := 0; ; try++ {
		err = add(ext, life)
		var se *soapError
		if errors.As(err, &se) && se.Code == 725 { // OnlyPermanentLeasesSupported
			life = 0
			err = add(ext, 0)
		}
		if !errors.As(err, &se) || (se.Code != 718 && se.Code != 606) || try == upnpPortTries-1 {
			break
		}
		ext++
	}
	if err != nil {
		return netip.AddrPort{}, 0, err
	}
	u.ext = ext
	return netip.AddrPortFrom(ip, ext), life, nil
}

// upnpPortTries is how many external ports we try when ours is taken.
const upnpPortTries = 10

func (u *upnp) unmap(ctx context.Context, internal uint16) error {
	ext := internal
	if u.ext != 0 {
		ext = u.ext
	}
	_, err := u.call(ctx, "DeletePortMapping", [][2]string{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(int(ext))},
		{"NewProtocol", "UDP"},
	})
	return err
}
