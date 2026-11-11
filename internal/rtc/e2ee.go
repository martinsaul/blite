package rtc

import (
	"errors"
	"sync"
	"time"

	"blite/internal/e2ee"
)

// End-to-end media encryption, when the room has it on
// (Welcome.E2EE). Each member encrypts what it sends with its own sender
// key and gives that key to every other member, sealed to the member's
// exchange key. Exchange keys are signed by identity keys, and both kinds
// of message travel as signals through the server, which can't read them.
// Relays forward media unchanged; a relay that decodes (transcoding) is a
// member and holds the keys like everyone else.
//
//   - On joining, we send our exchange key to everyone; members send theirs
//     to a newcomer. Whoever gets a verified exchange key sends back its
//     current sender key.
//   - When a member leaves, everyone makes a new sender key, hands it out,
//     and switches to it after keySwitchDelay, so the one who left can't
//     decrypt what follows. Old keys stay usable for keyGrace.

const (
	keySwitchDelay = time.Second
	keyGrace       = 5 * time.Second
)

// kxMsg carries a member's exchange key and its identity signature.
type kxMsg struct {
	Pub string `json:"pub"`
	Sig string `json:"sig"`
}

// keyMsg carries a sender key sealed to the recipient.
type keyMsg struct {
	Sealed string `json:"sealed"`
}

type crypt struct {
	ex   *e2ee.Exchange
	ring *e2ee.Keyring

	mu      sync.Mutex
	cur     *e2ee.SenderKey   // what we encrypt with
	next    *e2ee.SenderKey   // handed out, switched to after keySwitchDelay
	peerKX  map[string]string // member -> verified exchange key
	kxSent  map[string]bool   // members we sent our exchange key to
	pending map[string]string // sealed keys that arrived before the sender's exchange key
	owner   map[uint32]string // key id -> member
}

func newCrypt() *crypt {
	c := &crypt{ex: e2ee.NewExchange(), ring: e2ee.NewKeyring(), cur: e2ee.NewSenderKey(),
		peerKX: map[string]string{}, kxSent: map[string]bool{}, pending: map[string]string{}, owner: map[uint32]string{}}
	c.ring.Add(c.cur.KID, c.cur.Key) // we decode our own frames too (a relay's renditions loop back in tests)
	return c
}

func (c *crypt) sender() *e2ee.SenderKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

// sendKX sends our exchange key to peer, once.
func (m *Mesh) sendKX(peer string) {
	c := m.crypt
	c.mu.Lock()
	if c.kxSent[peer] {
		c.mu.Unlock()
		return
	}
	c.kxSent[peer] = true
	c.mu.Unlock()
	pub := c.ex.Public()
	sig := e2ee.SignExchange(m.opt.Identity.Private, m.sig.Welcome.Room, pub)
	m.sig.Signal(peer, signalMsg{KX: &kxMsg{Pub: pub, Sig: sig}})
}

// gotKX verifies peer's exchange key, answers with ours if we haven't sent
// it, and sends our sender key(s).
func (m *Mesh) gotKX(peer string, kx kxMsg) {
	m.mu.Lock()
	idKey := m.idKeys[peer]
	m.mu.Unlock()
	if !e2ee.VerifyExchange(idKey, m.sig.Welcome.Room, kx.Pub, kx.Sig) {
		m.opt.Log.Warn("e2ee: exchange key with a bad signature", "peer", peer)
		return
	}
	c := m.crypt
	c.mu.Lock()
	c.peerKX[peer] = kx.Pub
	sealed, waiting := c.pending[peer]
	delete(c.pending, peer)
	keys := []*e2ee.SenderKey{c.cur}
	if c.next != nil {
		keys = append(keys, c.next)
	}
	c.mu.Unlock()
	m.sendKX(peer)
	for _, k := range keys {
		m.sendKey(peer, kx.Pub, k)
	}
	if waiting {
		m.gotKey(peer, keyMsg{Sealed: sealed})
	}
}

func (m *Mesh) sendKey(peer, pub string, k *e2ee.SenderKey) {
	sealed, err := m.crypt.ex.SealKey(pub, k)
	if err != nil {
		m.opt.Log.Warn("e2ee: sealing key", "peer", peer, "err", err)
		return
	}
	m.sig.Signal(peer, signalMsg{Key: &keyMsg{Sealed: sealed}})
}

// gotKey installs peer's sender key and asks it for a keyframe, since its
// frames so far were undecryptable.
func (m *Mesh) gotKey(peer string, k keyMsg) {
	c := m.crypt
	c.mu.Lock()
	pub, ok := c.peerKX[peer]
	if !ok {
		c.pending[peer] = k.Sealed
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	kid, key, err := c.ex.OpenKey(pub, k.Sealed)
	if err != nil {
		m.opt.Log.Warn("e2ee: can't open sender key", "peer", peer, "err", err)
		return
	}
	c.mu.Lock()
	if o, taken := c.owner[kid]; taken && o != peer {
		c.mu.Unlock()
		m.opt.Log.Warn("e2ee: key id already used by another member", "peer", peer, "owner", o)
		return
	}
	c.owner[kid] = peer
	c.mu.Unlock()
	c.ring.Add(kid, key)
	m.opt.Log.Info("e2ee: got sender key", "peer", peer)
	m.pliUpstream(peer, srcScreen)
}

// cryptLeft forgets a member that left, retires its keys and replaces
// ours.
func (m *Mesh) cryptLeft(peer string) {
	c := m.crypt
	c.mu.Lock()
	delete(c.peerKX, peer)
	delete(c.kxSent, peer)
	delete(c.pending, peer)
	for kid, o := range c.owner {
		if o == peer {
			c.ring.Retire(kid, keyGrace)
			delete(c.owner, kid)
		}
	}
	next := e2ee.NewSenderKey()
	c.next = next
	c.ring.Add(next.KID, next.Key)
	peers := make(map[string]string, len(c.peerKX))
	for p, pub := range c.peerKX {
		peers[p] = pub
	}
	c.mu.Unlock()
	for p, pub := range peers {
		m.sendKey(p, pub, next)
	}
	time.AfterFunc(keySwitchDelay, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.next != next {
			return // a later leave replaced it
		}
		old := c.cur
		c.cur, c.next = next, nil
		c.ring.Retire(old.KID, keyGrace)
	})
}

// encVideo and encAudio encrypt what we send, when the room is encrypted.
func (m *Mesh) encVideo(au []byte) []byte {
	if m.crypt == nil {
		return au
	}
	return m.crypt.sender().EncryptVideo(au)
}

func (m *Mesh) encAudio(f []byte) []byte {
	if m.crypt == nil {
		return f
	}
	return m.crypt.sender().EncryptAudio(f)
}

// decVideo and decAudio decrypt what we received; ok false = drop it.
func (m *Mesh) decVideo(au []byte) ([]byte, bool) {
	if m.crypt == nil {
		return au, true
	}
	out, err := m.crypt.ring.DecryptVideo(au)
	if err != nil {
		m.logDecrypt(err)
		return nil, false
	}
	return out, true
}

func (m *Mesh) decAudio(f []byte) ([]byte, bool) {
	if m.crypt == nil {
		return f, true
	}
	out, err := m.crypt.ring.DecryptAudio(f)
	if err != nil {
		m.logDecrypt(err)
		return nil, false
	}
	return out, true
}

var decryptLog struct {
	sync.Mutex
	at time.Time
}

// logDecrypt notes undecryptable frames, at most every 10 s: missing keys
// are normal for a moment after a join; tampered frames are not.
func (m *Mesh) logDecrypt(err error) {
	decryptLog.Lock()
	defer decryptLog.Unlock()
	if time.Since(decryptLog.at) < 10*time.Second {
		return
	}
	decryptLog.at = time.Now()
	if errors.Is(err, e2ee.ErrNoKey) {
		m.opt.Log.Info("e2ee: dropping frames until the sender's key arrives")
	} else {
		m.opt.Log.Warn("e2ee: dropping a frame that failed authentication", "err", err)
	}
}

// Encrypted reports whether the room's media is encrypted end to end.
func (m *Mesh) Encrypted() bool { return m.crypt != nil }

// ForgetKeysOf drops every sender key we hold from peer (tests: what a
// member without the keys sees).
func (m *Mesh) ForgetKeysOf(peer string) {
	if m.crypt == nil {
		return
	}
	c := m.crypt
	c.mu.Lock()
	defer c.mu.Unlock()
	for kid, o := range c.owner {
		if o == peer {
			c.ring.Retire(kid, 0)
			delete(c.owner, kid)
		}
	}
	delete(c.peerKX, peer)
}
