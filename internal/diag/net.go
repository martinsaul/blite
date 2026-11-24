package diag

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"sort"
	"sync/atomic"
	"time"
)

// NetResult is the way to the signaling server and an upload estimate.
type NetResult struct {
	Server     string    `json:"server"`
	Addrs      []string  `json:"addrs,omitempty"`
	DNSMs      float64   `json:"dnsMs"`
	ConnectMs  []float64 `json:"connectMs"` // TCP handshakes
	RequestMs  []float64 `json:"requestMs"` // HTTPS requests on one open connection
	ConnectMed float64   `json:"connectMedianMs"`
	ConnectMax float64   `json:"connectMaxMs"`
	RequestMed float64   `json:"requestMedianMs"`
	RequestMax float64   `json:"requestMaxMs"`
	UploadMbps float64   `json:"uploadMbps"`
	UploadTo   string    `json:"uploadTo,omitempty"`
	UploadMB   float64   `json:"uploadMB"`
	UploadNote string    `json:"uploadNote,omitempty"`
}

// uploadURL discards what is posted to it: Cloudflare's speed test, at the
// Cloudflare edge nearest the user (bellion.app is served through
// Cloudflare too). The signaling server has no such endpoint, and its
// report endpoint stores what it gets.
const uploadURL = "https://speed.cloudflare.com/__up"

func runNet(ctx context.Context, r *Report, p *printer, server string) error {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return fmt.Errorf("bad server URL %q", server)
	}
	scheme, port := "https", "443"
	if u.Scheme == "ws" || u.Scheme == "http" {
		scheme, port = "http", "80"
	}
	if u.Port() != "" {
		port = u.Port()
	}
	host := u.Hostname()
	res := &NetResult{Server: server}
	r.with(func() { r.Net = res })

	t := time.Now()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	res.DNSMs = ms(time.Since(t))
	if err != nil {
		return fmt.Errorf("DNS %s: %w", host, err)
	}
	res.Addrs = addrs
	p.f("   %s: %s (DNS %.0f ms)", host, addrs, res.DNSMs)

	samples := 8
	if r.Quick {
		samples = 4
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	for i := 0; i < samples && ctx.Err() == nil; i++ {
		t := time.Now()
		c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
		if err != nil {
			p.f("   TCP connect: %v", err)
			continue
		}
		res.ConnectMs = append(res.ConnectMs, ms(time.Since(t)))
		c.Close()
	}
	res.ConnectMed, res.ConnectMax = medMax(res.ConnectMs)
	p.f("   TCP connect to %s:%s (the nearest edge when it's behind a CDN): median %.0f ms, max %.0f ms over %d", host, port, res.ConnectMed, res.ConnectMax, len(res.ConnectMs))

	health := scheme + "://" + u.Host + "/healthz"
	get := func() (time.Duration, error) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, health, nil)
		t := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("%s: %s", health, resp.Status)
		}
		return time.Since(t), nil
	}
	if _, err := get(); err != nil { // opens the connection
		return fmt.Errorf("signaling server: %w", err)
	}
	for i := 0; i < samples && ctx.Err() == nil; i++ {
		d, err := get()
		if err != nil {
			p.f("   request: %v", err)
			continue
		}
		res.RequestMs = append(res.RequestMs, ms(d))
	}
	res.RequestMed, res.RequestMax = medMax(res.RequestMs)
	p.f("   HTTPS request to the signaling server (through its CDN, if any): median %.0f ms, max %.0f ms over %d", res.RequestMed, res.RequestMax, len(res.RequestMs))

	size := 8 << 20
	if r.Quick {
		size = 3 << 20
	}
	mbps, sent, secs, err := upload(ctx, size)
	res.UploadTo, res.UploadMB, res.UploadMbps = uploadURL, float64(sent)/(1<<20), mbps
	res.UploadNote = "to the nearest Cloudflare edge, not the signaling server (it has no endpoint that discards uploads); the path to the people you share with may be slower"
	if err != nil {
		res.UploadNote += "; " + err.Error()
	}
	p.f("   upload: %.1f Mbit/s (%.1f MB in %.1f s to %s)%s", mbps, res.UploadMB, secs, uploadURL, errNote(err))
	return nil
}

// upload posts size random bytes to uploadURL after a small warm-up post,
// and returns the rate. On a slow link it stops after 20 s and rates what
// went out by then.
func upload(ctx context.Context, size int) (mbps float64, sent int64, secs float64, err error) {
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}}
	defer c.CloseIdleConnections()
	body := make([]byte, size)
	for i := 0; i+8 <= len(body); i += 8 { // incompressible
		v := rand.Uint64()
		for j := 0; j < 8; j++ {
			body[i+j] = byte(v >> (8 * j))
		}
	}
	post := func(ctx context.Context, b []byte, n *atomic.Int64) error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, &countReader{b: b, n: n})
		req.ContentLength = int64(len(b))
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("%s", resp.Status)
		}
		return nil
	}
	var warm atomic.Int64
	if err := post(ctx, body[:256<<10], &warm); err != nil {
		return 0, 0, 0, err
	}
	var n atomic.Int64
	tctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	t := time.Now()
	err = post(tctx, body, &n)
	secs = time.Since(t).Seconds()
	sent = n.Load()
	if err != nil && tctx.Err() != nil && ctx.Err() == nil {
		err = fmt.Errorf("slow: only %d KB in %.0f s", sent>>10, secs)
	}
	if secs > 0 {
		mbps = float64(sent) * 8 / secs / 1e6
	}
	return mbps, sent, secs, err
}

// countReader counts the bytes the transport took.
type countReader struct {
	b []byte
	n *atomic.Int64
}

func (r *countReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	k := copy(p, r.b)
	r.b = r.b[k:]
	r.n.Add(int64(k))
	return k, nil
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func medMax(v []float64) (med, mx float64) {
	if len(v) == 0 {
		return 0, 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2], s[len(s)-1]
}
