package turn

import (
	"testing"
	"time"
)

// Expected values computed independently with:
//
//	printf '%s' '1700021600:peer1' | openssl dgst -sha1 -hmac 'secret' -binary | base64
func TestCoturnCredential(t *testing.T) {
	c := Coturn{
		URLs:   []string{"turn:turn.example:3478"},
		Secret: []byte("secret"),
		TTL:    6 * time.Hour,
		now:    func() time.Time { return time.Unix(1700000000, 0) },
	}
	s := c.ICEServers("peer1")
	if s[0].Username != "1700021600:peer1" {
		t.Fatalf("username %q", s[0].Username)
	}
	if want := "kCR35SLOSfw9arc/ER8tO71mAcI="; s[0].Credential != want {
		t.Fatalf("credential %q, want %q", s[0].Credential, want)
	}
}
