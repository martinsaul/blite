package portmap

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIGD serves a device description and the WANIPConnection control URL.
type fakeIGD struct {
	mu       sync.Mutex
	mappings map[string]string // external port -> internal client
	actions  []string
	permOnly bool
	// conflict606: answer a taken port with 606 Action not authorized, as
	// some routers do, instead of 718.
	conflict606 bool
}

const igdDesc = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
 <device><deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
  <deviceList><device><deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
   <deviceList><device><deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
    <serviceList><service>
     <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
     <controlURL>/ctl/IPConn</controlURL>
    </service></serviceList>
   </device></deviceList>
  </device></deviceList>
 </device>
</root>`

func (f *fakeIGD) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/rootDesc.xml" {
		io.WriteString(w, igdDesc)
		return
	}
	if r.URL.Path != "/ctl/IPConn" {
		http.NotFound(w, r)
		return
	}
	act := strings.Trim(r.Header.Get("SOAPAction"), `"`)
	act = act[strings.Index(act, "#")+1:]
	body, _ := io.ReadAll(r.Body)
	args, _ := parseSOAP(strings.NewReader(string(body)), 200)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, act)
	fault := func(code, desc string) {
		w.WriteHeader(500)
		io.WriteString(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>`+code+`</errorCode><errorDescription>`+desc+`</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`)
	}
	reply := func(inner string) {
		io.WriteString(w, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:`+act+`Response xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1">`+inner+`</u:`+act+`Response></s:Body></s:Envelope>`)
	}
	switch act {
	case "GetExternalIPAddress":
		reply("<NewExternalIPAddress>203.0.113.7</NewExternalIPAddress>")
	case "AddPortMapping":
		if args["NewProtocol"] != "UDP" {
			fault("402", "Invalid Args")
			return
		}
		if c, ok := f.mappings[args["NewExternalPort"]]; ok && c != args["NewInternalClient"] {
			if f.conflict606 {
				fault("606", "Action not authorized")
			} else {
				fault("718", "ConflictInMappingEntry")
			}
			return
		}
		if f.permOnly && args["NewLeaseDuration"] != "0" {
			fault("725", "OnlyPermanentLeasesSupported")
			return
		}
		f.mappings[args["NewExternalPort"]] = args["NewInternalClient"]
		reply("")
	case "DeletePortMapping":
		if _, ok := f.mappings[args["NewExternalPort"]]; !ok {
			fault("714", "NoSuchEntryInArray")
			return
		}
		delete(f.mappings, args["NewExternalPort"])
		reply("")
	default:
		fault("401", "Invalid Action")
	}
}

func TestUPnPConflict(t *testing.T) {
	for _, c606 := range []bool{false, true} {
		testUPnPConflict(t, c606)
	}
}

func testUPnPConflict(t *testing.T, conflict606 bool) {
	igd := &fakeIGD{mappings: map[string]string{"30471": "192.168.1.99"}, conflict606: conflict606} // another machine holds 30471
	srv := httptest.NewServer(igd)
	defer srv.Close()
	ctx := context.Background()
	u, err := newUPnP(ctx, srv.URL+"/rootDesc.xml")
	if err != nil {
		t.Fatal(err)
	}
	ext, _, err := u.mapPort(ctx, 30471, lease)
	if err != nil || ext.Port() != 30472 {
		t.Fatalf("external %v, %v; want port 30472", ext, err)
	}
	if ext, _, err = u.mapPort(ctx, 30471, lease); err != nil || ext.Port() != 30472 { // renewal keeps it
		t.Fatalf("renewal: %v, %v", ext, err)
	}
	if err := u.unmap(ctx, 30471); err != nil || igd.mappings["30471"] != "192.168.1.99" || igd.mappings["30472"] != "" {
		t.Fatalf("unmap: %v %v", err, igd.mappings)
	}
}

func TestUPnP(t *testing.T) {
	for _, perm := range []bool{false, true} {
		igd := &fakeIGD{mappings: map[string]string{}, permOnly: perm}
		srv := httptest.NewServer(igd)
		ctx := context.Background()
		u, err := newUPnP(ctx, srv.URL+"/rootDesc.xml")
		if err != nil {
			t.Fatal(err)
		}
		if u.ctrl != srv.URL+"/ctl/IPConn" {
			t.Fatalf("control URL %q", u.ctrl)
		}
		ext, life, err := u.mapPort(ctx, 30471, lease)
		if err != nil {
			t.Fatal(err)
		}
		if ext != netip.MustParseAddrPort("203.0.113.7:30471") {
			t.Fatalf("external %v", ext)
		}
		if want := map[bool]time.Duration{false: lease, true: 0}[perm]; life != want {
			t.Fatalf("permOnly=%v: lease %v, want %v", perm, life, want)
		}
		if igd.mappings["30471"] != "127.0.0.1" {
			t.Fatalf("mappings %v", igd.mappings)
		}
		if err := u.unmap(ctx, 30471); err != nil {
			t.Fatal(err)
		}
		if len(igd.mappings) != 0 {
			t.Fatalf("mapping left: %v", igd.mappings)
		}
		var se *soapError
		if err := u.unmap(ctx, 30471); !errors.As(err, &se) || se.Code != 714 {
			t.Fatalf("second unmap: %v", err)
		}
		srv.Close()
	}
}

// fakeSSDP starts a UDP responder on ip that replies to the first M-SEARCH
// with an SSDP 200 OK whose Location header is loc.
func fakeSSDP(t *testing.T, ip net.IP, loc string) netip.AddrPort {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			_, from, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			resp := "HTTP/1.1 200 OK\r\nLOCATION: " + loc + "\r\nST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n\r\n"
			c.WriteToUDPAddrPort([]byte(resp), from)
		}
	}()
	return c.LocalAddr().(*net.UDPAddr).AddrPort()
}

func TestDiscoverIGDFiltering(t *testing.T) {
	igd := &fakeIGD{mappings: map[string]string{}}
	srv := httptest.NewServer(igd)
	defer srv.Close()
	local := netip.MustParseAddr("127.0.0.1")

	// Happy path: sender IP == gateway IP == Location host IP.
	ssdp := fakeSSDP(t, net.IPv4(127, 0, 0, 1), srv.URL+"/rootDesc.xml")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	locs, err := discoverIGD(ctx, local, local, ssdp)
	cancel()
	if err != nil || len(locs) != 1 || locs[0] != srv.URL+"/rootDesc.xml" {
		t.Fatalf("happy discoverIGD: %v, %v", locs, err)
	}
	u, err := newUPnP(context.Background(), locs[0])
	if err != nil {
		t.Fatal(err)
	}
	if ext, _, err := u.mapPort(context.Background(), 30471, lease); err != nil || ext.Port() != 30471 {
		t.Fatalf("mapPort: %v, %v", ext, err)
	}

	// Answer from a non-gateway IP (127.0.0.1 != gateway 192.168.1.1) is ignored.
	ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
	_, err = discoverIGD(ctx, local, netip.MustParseAddr("192.168.1.1"), ssdp)
	cancel()
	if err == nil {
		t.Fatal("accepted SSDP answer from non-gateway IP")
	}

	// Location pointing elsewhere (different host IP than sender) is ignored.
	badLocSSDP := fakeSSDP(t, net.IPv4(127, 0, 0, 1), "http://192.168.1.1:1900/rootDesc.xml")
	ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
	_, err = discoverIGD(ctx, local, local, badLocSSDP)
	cancel()
	if err == nil {
		t.Fatal("accepted SSDP answer with Location on another host")
	}
}

func TestAllowedIGDFallback(t *testing.T) {
	nets := []netip.Prefix{
		netip.MustParsePrefix("192.168.1.0/24"),
		netip.MustParsePrefix("169.254.0.0/16"),
	}
	var noGW netip.Addr
	for _, tc := range []struct {
		src  string
		want bool
	}{
		{"192.168.1.1", true},
		{"169.254.10.1", true},
		{"192.168.2.1", false}, // private, not on our subnets
		{"10.0.0.1", false},    // private, not on our subnets
		{"127.0.0.1", false},   // loopback rejected even without gateway
		{"203.0.113.1", false}, // public address rejected
	} {
		if got := allowedIGD(netip.MustParseAddr(tc.src), noGW, nets); got != tc.want {
			t.Errorf("allowedIGD(%s) = %v, want %v", tc.src, got, tc.want)
		}
	}
	// Loopback is rejected even if a prefix somehow contained it.
	loopNet := []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
	if allowedIGD(netip.MustParseAddr("127.0.0.1"), noGW, loopNet) {
		t.Error("allowedIGD accepted loopback")
	}
}

func TestUPnPRedirectNotFollowed(t *testing.T) {
	targetHit := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHit = true
		io.WriteString(w, igdDesc)
	}))
	defer target.Close()

	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/rootDesc.xml", http.StatusFound)
	}))
	defer redir.Close()

	if _, err := newUPnP(context.Background(), redir.URL+"/rootDesc.xml"); err == nil {
		t.Fatal("newUPnP succeeded across redirect")
	}
	if targetHit {
		t.Fatal("redirect target was fetched")
	}
}

func TestUPnPControlURLValidation(t *testing.T) {
	descWith := func(urlBase, ctrl string) string {
		return `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">` + urlBase + `
 <device><deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
  <serviceList><service>
   <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
   <controlURL>` + ctrl + `</controlURL>
  </service></serviceList>
 </device>
</root>`
	}

	// Listen on 127.0.0.2 so we can test controlURL pointing to 127.0.0.1 (different IP) or another host.
	ln, err := net.Listen("tcp4", "127.0.0.2:0")
	if err != nil {
		// Fallback if 127.0.0.2 is unavailable: bind 127.0.0.1 and test against 127.0.0.2 and 192.168.1.99.
		ln, err = net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
	}
	srvHost, srvPort, _ := net.SplitHostPort(ln.Addr().String())
	otherLoop := "127.0.0.1"
	if srvHost == "127.0.0.1" {
		otherLoop = "127.0.0.2"
	}

	var curDesc string
	srv := &httptest.Server{
		Listener: ln,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, curDesc)
		})},
	}
	srv.Start()
	defer srv.Close()

	for _, tc := range []struct {
		name    string
		urlBase string
		ctrl    string
		wantOK  bool
	}{
		{"same host different port", "", "http://" + srvHost + ":9999/ctl", true},
		{"other host in controlURL", "", "http://192.168.1.99:1900/ctl", false},
		{"loopback mismatch in controlURL", "", "http://" + otherLoop + ":" + srvPort + "/ctl", false},
		{"other host in URLBase", "<URLBase>http://192.168.1.99:1900/</URLBase>", "/ctl", false},
		{"loopback mismatch in URLBase", "<URLBase>http://" + otherLoop + ":" + srvPort + "/</URLBase>", "/ctl", false},
		{"https scheme in controlURL", "", "https://" + srvHost + ":" + srvPort + "/ctl", false},
		{"hostname in controlURL", "", "http://localhost:" + srvPort + "/ctl", false},
	} {
		curDesc = descWith(tc.urlBase, tc.ctrl)
		_, err := newUPnP(context.Background(), srv.URL+"/rootDesc.xml")
		if (err == nil) != tc.wantOK {
			t.Errorf("%s: err = %v, wantOK = %v", tc.name, err, tc.wantOK)
		}
	}
}

func TestUPnPServiceTypeValidation(t *testing.T) {
	var curSvc string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
 <device><serviceList><service>
  <serviceType>`+curSvc+`</serviceType>
  <controlURL>/ctl</controlURL>
 </service></serviceList></device>
</root>`)
	}))
	defer srv.Close()

	for _, good := range []string{
		"urn:schemas-upnp-org:service:WANIPConnection:1",
		"urn:schemas-upnp-org:service:WANIPConnection:2",
		"urn:schemas-upnp-org:service:WANPPPConnection:1",
	} {
		curSvc = good
		if _, err := newUPnP(context.Background(), srv.URL+"/rootDesc.xml"); err != nil {
			t.Errorf("rejected valid service %q: %v", good, err)
		}
	}

	for _, bad := range []string{
		"urn:schemas-upnp-org:service:WANIPConnection:3",
		"urn:schemas-upnp-org:service:WANPPPConnection:2",
		"urn:schemas-upnp-org:service:WANIPConnection:1\"><injected/>",
		"urn:schemas-upnp-org:service:WANIPConnection:1\r\nX-Injected: 1",
		"urn:schemas-upnp-org:service:OtherService:1",
	} {
		curSvc = bad
		if _, err := newUPnP(context.Background(), srv.URL+"/rootDesc.xml"); err == nil {
			t.Errorf("accepted unknown service %q", bad)
		}
	}
}

// fakePMP answers NAT-PMP, and PCP unless pcp is false.
func fakePMP(t *testing.T, pcp bool) (netip.AddrPort, *sync.Map) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	maps := &sync.Map{} // internal port -> lifetime
	go func() {
		buf := make([]byte, 1100)
		for {
			n, from, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			b := buf[:n]
			switch {
			case b[0] == 0 && b[1] == 0:
				r := make([]byte, 12)
				r[1] = 128
				copy(r[8:], []byte{198, 51, 100, 9})
				c.WriteToUDPAddrPort(r, from)
			case b[0] == 0 && b[1] == 1:
				in, life := binary.BigEndian.Uint16(b[4:]), binary.BigEndian.Uint32(b[8:])
				if life == 0 {
					maps.Delete(in)
				} else {
					maps.Store(in, life)
				}
				r := make([]byte, 16)
				r[1] = 129
				binary.BigEndian.PutUint16(r[8:], in)
				binary.BigEndian.PutUint16(r[10:], in+1) // gateway picks another port
				binary.BigEndian.PutUint32(r[12:], min(life, 3600))
				c.WriteToUDPAddrPort(r, from)
			case b[0] == 2 && !pcp:
				r := make([]byte, 8)
				binary.BigEndian.PutUint16(r[2:], 1) // unsupported version
				r[1] = 128 + b[1]
				c.WriteToUDPAddrPort(r, from)
			case b[0] == 2 && b[1] == 1 && n == 60:
				in, life := binary.BigEndian.Uint16(b[40:]), binary.BigEndian.Uint32(b[4:])
				if life == 0 {
					maps.Delete(in)
				} else {
					maps.Store(in, life)
				}
				r := make([]byte, 60)
				copy(r, b)
				r[1] = 0x81
				r[3] = 0
				binary.BigEndian.PutUint32(r[4:], min(life, 3600))
				ext := netip.MustParseAddr("198.51.100.10").As16()
				copy(r[44:], ext[:])
				c.WriteToUDPAddrPort(r, from)
			}
		}
	}()
	return c.LocalAddr().(*net.UDPAddr).AddrPort(), maps
}

func TestPMP(t *testing.T) {
	for _, tc := range []struct {
		pcp  bool
		name string
		ext  string
	}{{true, "PCP", "198.51.100.10:30471"}, {false, "NAT-PMP", "198.51.100.9:30472"}} {
		gw, maps := fakePMP(t, tc.pcp)
		p := &pmp{pcp: newPCP(gw, netip.MustParseAddr("127.0.0.1")), natpmp: natpmp{gw: gw}}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ext, life, err := p.mapPort(ctx, 30471, lease)
		cancel()
		if err != nil {
			t.Fatal(tc.name, err)
		}
		if p.name() != tc.name || ext.String() != tc.ext || life != time.Hour {
			t.Fatalf("%s: got %s %v %v", tc.name, p.name(), ext, life)
		}
		if _, ok := maps.Load(uint16(30471)); !ok {
			t.Fatalf("%s: not mapped", tc.name)
		}
		if err := p.unmap(context.Background(), 30471); err != nil {
			t.Fatal(err)
		}
		if _, ok := maps.Load(uint16(30471)); ok {
			t.Fatalf("%s: still mapped", tc.name)
		}
	}
}

func TestPMPTimeout(t *testing.T) {
	c, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}) // never answers
	defer c.Close()
	gw := c.LocalAddr().(*net.UDPAddr).AddrPort()
	p := &pmp{pcp: newPCP(gw, netip.MustParseAddr("127.0.0.1")), natpmp: natpmp{gw: gw}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, _, err := p.mapPort(ctx, 30471, lease); err == nil {
		t.Fatal("mapped with a silent gateway")
	}
}

type fakeMapper struct {
	mu     sync.Mutex
	fails  bool
	maps   int
	unmaps int
}

func (*fakeMapper) name() string      { return "UPnP" }
func (*fakeMapper) local() netip.Addr { return netip.MustParseAddr("192.168.1.20") }
func (f *fakeMapper) mapPort(ctx context.Context, in uint16, life time.Duration) (netip.AddrPort, time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails {
		return netip.AddrPort{}, 0, errors.New("refused")
	}
	f.maps++
	return netip.AddrPortFrom(netip.MustParseAddr("203.0.113.7"), in), life, nil
}
func (f *fakeMapper) unmap(context.Context, uint16) error {
	f.mu.Lock()
	f.unmaps++
	f.mu.Unlock()
	return nil
}

func TestMapper(t *testing.T) {
	fm := &fakeMapper{}
	states := make(chan State, 10)
	m := newMapper(30471, nil, func(s State) { states <- s })
	m.find = func(context.Context) []func(context.Context) (mapper, error) {
		return []func(context.Context) (mapper, error){
			func(context.Context) (mapper, error) { return nil, errors.New("no PCP") },
			func(context.Context) (mapper, error) { return fm, nil },
		}
	}
	m.run()
	s := <-states
	if !s.Usable() || s.Method != "UPnP" || s.External.String() != "203.0.113.7:30471" || s.Local.String() != "192.168.1.20" || s.Port != 30471 {
		t.Fatalf("state %+v", s)
	}
	m.Close()
	if fm.unmaps != 1 {
		t.Fatalf("unmapped %d times", fm.unmaps)
	}
}

func TestMapperNoRouter(t *testing.T) {
	states := make(chan State, 10)
	m := newMapper(30471, nil, func(s State) { states <- s })
	m.find = func(context.Context) []func(context.Context) (mapper, error) {
		return []func(context.Context) (mapper, error){
			func(context.Context) (mapper, error) { return &fakeMapper{fails: true}, nil },
		}
	}
	m.run()
	s := <-states
	if s.Usable() || s.Trying || (s.Err == "" && !s.Public) {
		t.Fatalf("state %+v", s)
	}
	m.Close()
}

func TestPublicAddr(t *testing.T) {
	for ip, want := range map[string]bool{"203.0.113.7": true, "10.0.0.5": false, "100.64.0.1": false, "192.168.1.1": false, "fe80::1": false} {
		if publicAddr(netip.MustParseAddr(ip)) != want {
			t.Errorf("publicAddr(%s) != %v", ip, want)
		}
	}
}
