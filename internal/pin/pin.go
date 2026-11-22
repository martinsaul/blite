// Package pin pins the certificate authorities the signaling server's TLS
// certificate may come from. The built-in server sits behind Cloudflare,
// which issues and rotates its edge certificate itself, from Let's Encrypt,
// Google Trust Services or SSL.com; pinning the certificate or an
// intermediate would lock everyone out at its next rotation. So the pins
// are those CAs' roots (roots.pem): a certificate that chains only to some
// other root the computer trusts (a company proxy, antivirus HTTPS
// scanning, malware) is refused.
//
// The built-in server is always pinned; the user's own servers only when
// they turn it on (they may use another CA). Install hooks the check into
// http.DefaultTransport, which the signaling socket, reports, room probes
// and Discord requests all use.
package pin

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed roots.pem
var rootsPEM []byte

var roots = func() *x509.CertPool {
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(rootsPEM) {
		panic("pin: no roots")
	}
	return p
}()

// ErrUnpinned: the server's certificate doesn't chain to a pinned root.
var ErrUnpinned = errors.New("the server's certificate isn't from a trusted public authority: something on this computer or network is intercepting the connection (a company proxy, or antivirus HTTPS scanning)")

// now is the time certificates are checked at (tests set it).
var now = time.Now

var (
	mu     sync.RWMutex
	pinned = func(host string) bool { return false }
)

// SetPinned sets which hosts are pinned (host names, lower case, no port).
func SetPinned(f func(host string) bool) {
	mu.Lock()
	pinned = f
	mu.Unlock()
}

// Pinned reports whether host is pinned.
func Pinned(host string) bool {
	mu.RLock()
	f := pinned
	mu.RUnlock()
	return f(strings.ToLower(host))
}

// Verify checks a TLS connection to a pinned host against the pinned roots
// (after the usual checks against the system's). Other hosts pass.
func Verify(cs tls.ConnectionState) error {
	if !Pinned(cs.ServerName) {
		return nil
	}
	if len(cs.PeerCertificates) == 0 {
		return ErrUnpinned
	}
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: inter,
		DNSName:       cs.ServerName,
		CurrentTime:   now(),
	})
	if err != nil {
		return fmt.Errorf("%w (%s, issued by %s)", ErrUnpinned, cs.ServerName, cs.PeerCertificates[0].Issuer.CommonName)
	}
	return nil
}

// Install adds Verify to t's TLS settings.
func Install(t *http.Transport) {
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{}
	}
	prev := t.TLSClientConfig.VerifyConnection
	t.TLSClientConfig.VerifyConnection = func(cs tls.ConnectionState) error {
		if prev != nil {
			if err := prev(cs); err != nil {
				return err
			}
		}
		return Verify(cs)
	}
}
