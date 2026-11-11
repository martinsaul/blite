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
// that answer before ctx ends (at least one, or an error).
func discoverIGD(ctx context.Context, local netip.Addr) ([]string, error) {
	c, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(local, 0)))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	for _, st := range searchTargets {
		msg := "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\nST: " + st + "\r\n\r\n"
		if _, err := c.WriteToUDPAddrPort([]byte(msg), ssdpAddr); err != nil {
			return nil, err
		}
	}
	// Collect for a short while once the first answer arrived: a router may
	// answer each search target, and other devices answer too.
	var locs []string
	seen := map[string]bool{}
	end, _ := ctx.Deadline()
	buf := make([]byte, 2048)
	for {
		c.SetReadDeadline(end)
		n, err := c.Read(buf)
		if err != nil {
			break
		}
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf[:n])), nil)
		if err != nil {
			continue
		}
		loc := resp.Header.Get("Location")
		if loc != "" && !seen[loc] {
			seen[loc] = true
			locs = append(locs, loc)
			if soon := time.Now().Add(300 * time.Millisecond); soon.Before(end) {
				end = soon
			}
		}
	}
	if len(locs) == 0 {
		return nil, errors.New("no UPnP gateway answered")
	}
	return locs, nil
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

type upnpDevice struct {
	Services []struct {
		ServiceType string `xml:"serviceType"`
		ControlURL  string `xml:"controlURL"`
	} `xml:"serviceList>service"`
	Devices []upnpDevice `xml:"deviceList>device"`
}

// findWAN returns the first WAN connection service in the tree, preferring
// WANIPConnection.
func (d *upnpDevice) findWAN(kind string) (svc, ctrl string) {
	for _, s := range d.Services {
		if strings.HasPrefix(s.ServiceType, "urn:schemas-upnp-org:service:"+kind+":") {
			return s.ServiceType, s.ControlURL
		}
	}
	for i := range d.Devices {
		if svc, ctrl = d.Devices[i].findWAN(kind); svc != "" {
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
	host, err := netip.ParseAddr(u.Hostname())
	if err != nil {
		return nil, fmt.Errorf("UPnP location %q: %w", loc, err)
	}
	local, err := localIPTo(host)
	if err != nil {
		return nil, err
	}
	cl := &http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, "GET", loc, nil)
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var root struct {
		URLBase string     `xml:"URLBase"`
		Device  upnpDevice `xml:"device"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&root); err != nil {
		return nil, fmt.Errorf("UPnP description: %w", err)
	}
	svc, ctrl := root.Device.findWAN("WANIPConnection")
	if svc == "" {
		svc, ctrl = root.Device.findWAN("WANPPPConnection")
	}
	if svc == "" {
		return nil, errors.New("UPnP device has no WAN connection service")
	}
	base := u
	if root.URLBase != "" {
		if b, err := url.Parse(root.URLBase); err == nil {
			base = b
		}
	}
	c, err := base.Parse(ctrl)
	if err != nil {
		return nil, err
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
	b.WriteString(`<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&b, `<u:%s xmlns:u="%s">`, action, u.service)
	for _, a := range args {
		fmt.Fprintf(&b, "<%s>", a[0])
		xml.EscapeText(&b, []byte(a[1]))
		fmt.Fprintf(&b, "</%s>", a[0])
	}
	fmt.Fprintf(&b, `</u:%s></s:Body></s:Envelope>`, action)
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
	return parseSOAP(io.LimitReader(resp.Body, 1<<16), resp.StatusCode)
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
	// ConflictInMappingEntry): try the next ones. A renewal asks for the
	// one we got.
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
		if !errors.As(err, &se) || se.Code != 718 || try == upnpPortTries-1 {
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
