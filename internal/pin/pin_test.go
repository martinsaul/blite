package pin

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func chain(t *testing.T, file string) []*x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var cs []*x509.Certificate
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return cs
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
}

func TestRealChain(t *testing.T) {
	// bellion.app as Cloudflare served it (Let's Encrypt YE2), checked
	// while that certificate was valid.
	defer func(f func() time.Time) { now = f }(now)
	now = func() time.Time { return time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC) }
	SetPinned(func(h string) bool { return h == "bellion.app" })
	defer SetPinned(func(string) bool { return false })
	cs := tls.ConnectionState{ServerName: "bellion.app", PeerCertificates: chain(t, "testdata/bellion-app-chain.pem")}
	if err := Verify(cs); err != nil {
		t.Fatal(err)
	}
	// Without the intermediates there's no path to a pinned root.
	cs.PeerCertificates = cs.PeerCertificates[:1]
	if err := Verify(cs); !errors.Is(err, ErrUnpinned) {
		t.Fatalf("leaf alone: %v", err)
	}
}

func TestInterceptedRefused(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	// The computer trusts the interceptor's root (as a company proxy's).
	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.ServerName = "example.com"
	Install(tr)
	c := &http.Client{Transport: tr}

	SetPinned(func(string) bool { return false })
	if _, err := c.Get(srv.URL); err != nil {
		t.Fatalf("unpinned host: %v", err)
	}
	SetPinned(func(h string) bool { return h == "example.com" })
	defer SetPinned(func(string) bool { return false })
	tr.CloseIdleConnections()
	if _, err := c.Get(srv.URL); !errors.Is(err, ErrUnpinned) {
		t.Fatalf("pinned host behind an interceptor: %v", err)
	}
}

func TestRoots(t *testing.T) {
	n := 0
	for b := rootsPEM; ; n++ {
		var blk *pem.Block
		if blk, b = pem.Decode(b); blk == nil {
			break
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil || !c.IsCA {
			t.Fatalf("root %d: %v", n, err)
		}
		if time.Until(c.NotAfter) < 365*24*time.Hour {
			t.Errorf("%s expires %s: refresh roots.pem", c.Subject.CommonName, c.NotAfter.Format(time.DateOnly))
		}
	}
	if n != 12 {
		t.Fatalf("%d roots", n)
	}
}
