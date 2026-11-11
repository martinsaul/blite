package discord

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"testing"
)

// fakeDiscord answers like the Discord app's RPC socket, on the connection
// accept gives (fakeListen, per OS).
func fakeDiscord(t *testing.T) {
	accept := fakeListen(t)
	go func() {
		c, err := accept()
		if err != nil {
			return
		}
		defer c.Close()
		send := func(op uint32, v any) {
			b, _ := json.Marshal(v)
			h := make([]byte, 8)
			binary.LittleEndian.PutUint32(h, op)
			binary.LittleEndian.PutUint32(h[4:], uint32(len(b)))
			c.Write(append(h, b...))
		}
		h := make([]byte, 8)
		for {
			if _, err := io.ReadFull(c, h); err != nil {
				return
			}
			b := make([]byte, binary.LittleEndian.Uint32(h[4:]))
			io.ReadFull(c, b)
			var m map[string]any
			json.Unmarshal(b, &m)
			if binary.LittleEndian.Uint32(h) == opHandshake {
				send(opFrame, map[string]any{"cmd": "DISPATCH", "evt": "READY", "data": map[string]any{"v": 1}})
				continue
			}
			n := m["nonce"]
			switch m["cmd"] {
			case "AUTHORIZE":
				send(opFrame, map[string]any{"cmd": "AUTHORIZE", "nonce": n, "data": map[string]any{"code": "c0de"}})
			case "AUTHENTICATE":
				if m["args"].(map[string]any)["access_token"] != "good" {
					send(opFrame, map[string]any{"cmd": "AUTHENTICATE", "evt": "ERROR", "nonce": n, "data": map[string]any{"code": 4009, "message": "Invalid OAuth2 access token"}})
					continue
				}
				send(opFrame, map[string]any{"cmd": "AUTHENTICATE", "nonce": n, "data": map[string]any{}})
			case "GET_SELECTED_VOICE_CHANNEL":
				send(opFrame, map[string]any{"cmd": m["cmd"], "nonce": n, "data": map[string]any{"id": "111222333", "name": "general", "guild_id": "999888777"}})
			case "SUBSCRIBE":
				send(opFrame, map[string]any{"cmd": "SUBSCRIBE", "nonce": n, "data": map[string]any{"evt": m["evt"]}})
				send(opFrame, map[string]any{"cmd": "DISPATCH", "evt": "VOICE_CHANNEL_SELECT", "data": map[string]any{"channel_id": nil}})
			}
		}
	}()
}

func TestRPC(t *testing.T) {
	fakeDiscord(t)
	c, err := Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if code, err := c.Authorize(); err != nil || code != "c0de" {
		t.Fatalf("Authorize = %q %v", code, err)
	}
	if err := c.Authenticate("bad"); err == nil {
		t.Fatal("bad token accepted")
	}
	if err := c.Authenticate("good"); err != nil {
		t.Fatal(err)
	}
	ch, err := c.VoiceChannel()
	if err != nil || ch == nil || ch.ID != "111222333" || ch.Name != "general" || ch.GuildID != "999888777" {
		t.Fatalf("VoiceChannel = %+v %v", ch, err)
	}
	if err := c.WatchVoice(); err != nil {
		t.Fatal(err)
	}
	if e := <-c.Events(); e.Evt != "VOICE_CHANNEL_SELECT" {
		t.Fatalf("event %+v", e)
	}
}
