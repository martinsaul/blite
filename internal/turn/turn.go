// Package turn hands out ICE servers to peers. STUN-only today; coturn
// and hosted TURN services plug in as Providers later.
package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strconv"
	"time"

	"blite/internal/proto"
)

// Provider returns the ICE servers a given peer should use.
type Provider interface {
	ICEServers(peerID string) []proto.ICEServer
}

// Static returns a fixed list (e.g. public STUN servers).
type Static []proto.ICEServer

func (s Static) ICEServers(string) []proto.ICEServer { return s }

// DefaultSTUN is used when nothing else is configured.
var DefaultSTUN = Static{{URLs: []string{"stun:stun.l.google.com:19302", "stun:stun.cloudflare.com:3478"}}}

// Coturn issues coturn "TURN REST API" credentials (use-auth-secret):
// username = "<expiry unix>:<peerID>", credential = base64(HMAC-SHA1(secret, username)).
type Coturn struct {
	URLs   []string
	Secret []byte
	TTL    time.Duration
	Extra  []proto.ICEServer // e.g. STUN entries
	now    func() time.Time
}

func (c Coturn) ICEServers(peerID string) []proto.ICEServer {
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	user := strconv.FormatInt(now().Add(c.TTL).Unix(), 10) + ":" + peerID
	mac := hmac.New(sha1.New, c.Secret)
	mac.Write([]byte(user))
	cred := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return append([]proto.ICEServer{{URLs: c.URLs, Username: user, Credential: cred}}, c.Extra...)
}
