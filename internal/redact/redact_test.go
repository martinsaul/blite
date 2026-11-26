package redact

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestArgs(t *testing.T) {
	orig := []string{
		"bshare://join/K7PQ2M4X#s3cretPw",
		"-background",
	}
	got := Args(orig)
	want := []string{
		"bshare://join/K7PQ2M4X#[cut]",
		"-background",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Args = %q, want %q", got, want)
	}
	if orig[0] != "bshare://join/K7PQ2M4X#s3cretPw" {
		t.Fatalf("Args mutated input slice: %q", orig)
	}

	cases := []struct {
		in   []string
		want []string
	}{
		{
			in:   []string{"blite://join/K7PQ-2M4X?server=share.example.com# hunter2 "},
			want: []string{"blite://join/K7PQ-2M4X?server=share.example.com#[cut]"},
		},
		{
			in:   []string{"https://share.bellion.io/j/K7PQ2M4X#myS3cret"},
			want: []string{"https://share.bellion.io/j/K7PQ2M4X#[cut]"},
		},
		{
			in:   []string{"https://share.bellion.io/custom/room/K7PQ2M4X%23myS3cret"},
			want: []string{"https://share.bellion.io/custom/room/K7PQ2M4X#[cut]"},
		},
		{
			in:   []string{"share.bellion.io/r/K7PQ2M4X#myS3cret"},
			want: []string{"share.bellion.io/r/K7PQ2M4X#[cut]"},
		},
		{
			in:   []string{"K7PQ-2M4X#myS3cret", "k7pq 2m4x#pw2", "MAMACO%23pw3"},
			want: []string{"K7PQ-2M4X#[cut]", "k7pq 2m4x#[cut]", "MAMACO#[cut]"},
		},
		{
			in:   []string{"-headless", "-link", "https://share.bellion.io/j/7K3QX9MD#topSecret", "-view", "count"},
			want: []string{"-headless", "-link", "https://share.bellion.io/j/7K3QX9MD#[cut]", "-view", "count"},
		},
		{
			in:   []string{"-link=bshare://join/7K3QX9MD#topSecret", "-password= swordfish ", "-name", "alice#7F2A"},
			want: []string{"-link=bshare://join/7K3QX9MD#[cut]", "-password=[cut]", "-name", "alice#7F2A"},
		},
		{
			in:   []string{"-join", "7K3QX9MD#joinPw", "-Password", "topSecret", "--TOKEN", "bearerTok", "-RoomPassword", "rp1", "--room_password=rp2", "-pass", "p1", "--key=k1", "-SECRET", "s1", "--api-key", "ak1", "-apikey=ak2", "--access-token", "at1", "-x-token=xt1", "-passwd", "pwd1"},
			want: []string{"-join", "7K3QX9MD#[cut]", "-Password", "[cut]", "--TOKEN", "[cut]", "-RoomPassword", "[cut]", "--room_password=[cut]", "-pass", "[cut]", "--key=[cut]", "-SECRET", "[cut]", "--api-key", "[cut]", "-apikey=[cut]", "--access-token", "[cut]", "-x-token=[cut]", "-passwd", "[cut]"},
		},
		{
			in:   []string{"-server", "wss://us%40er:p@ss31@share.example.com/ws?token=t99&key=k99"},
			want: []string{"-server", "wss://[cut]@share.example.com/ws?token=[cut]&key=[cut]"},
		},
		{
			in:   []string{"bshare://join/7K3QX9MD"}, // no fragment: keep intact
			want: []string{"bshare://join/7K3QX9MD"},
		},
	}
	for _, tc := range cases {
		if got := Args(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Args(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestText(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		secrets []string
	}{
		{
			name:    "guard args bshare",
			in:      `2026-11-15T10:00:00Z guard: started C:\Users\alice\AppData\Local\Blite\bin\blite.exe pid 4120 args ["bshare://join/K7PQ2M4X#s3cretPw" "-background"]`,
			want:    `2026-11-15T10:00:00Z guard: started C:\Users\alice\AppData\Local\Blite\bin\blite.exe pid 4120 args ["bshare://join/K7PQ2M4X#[cut]" "-background"]`,
			secrets: []string{"s3cretPw"},
		},
		{
			name:    "guard args passphrase with spaces and escaped quotes",
			in:      fmt.Sprintf("guard: started blite.exe pid 99 args %q", []string{`bshare://join/K7PQ2M4X?server=share.example.com#correct "horse" battery`, "-background"}),
			want:    `guard: started blite.exe pid 99 args ["bshare://join/K7PQ2M4X?server=share.example.com#[cut]" "-background"]`,
			secrets: []string{"horse", "battery"},
		},
		{
			name:    "guard args https share link",
			in:      `2026-11-15T10:00:00Z guard: started blite.exe pid 100 args ["https://share.bellion.io/j/7K3QX9MD#roomPass99"]`,
			want:    `2026-11-15T10:00:00Z guard: started blite.exe pid 100 args ["https://share.bellion.io/j/7K3QX9MD#[cut]"]`,
			secrets: []string{"roomPass99"},
		},
		{
			name:    "guard args link flag",
			in:      `guard: started blite.exe pid 100 args ["-link=blite://join/7K3QX9MD#roomPass99" "-password" "cliSecret"]`,
			want:    `guard: started blite.exe pid 100 args ["-link=blite://join/7K3QX9MD#[cut]" "-password" "[cut]"]`,
			secrets: []string{"roomPass99", "cliSecret"},
		},
		{
			name:    "slog unquoted secret fields case insensitive widened keys and link keeps code",
			in:      `time=2026-11-15T10:00:00Z level=INFO msg="room open" room=7K3Q-X9MD LINK=https://share.bellion.io/j/7K3QX9MD#ultrasecret Password=ultrasecret roomPassword=rpVal1 room_password=rpVal2 PW=p1 Pass=passVal3 Token=tok99 Key=keyVal4 SECRET=secVal5 api_key=akVal6 apikey=akVal7 access_token=atVal8 x-token=xtVal9 passwd=pwdVal10 peers=3`,
			want:    `time=2026-11-15T10:00:00Z level=INFO msg="room open" room=7K3Q-X9MD LINK=https://share.bellion.io/j/7K3QX9MD#[cut] Password=[cut] roomPassword=[cut] room_password=[cut] PW=[cut] Pass=[cut] Token=[cut] Key=[cut] SECRET=[cut] api_key=[cut] apikey=[cut] access_token=[cut] x-token=[cut] passwd=[cut] peers=3`,
			secrets: []string{"ultrasecret", "rpVal1", "rpVal2", "passVal3", "tok99", "keyVal4", "secVal5", "akVal6", "akVal7", "atVal8", "xtVal9", "pwdVal10"},
		},
		{
			name:    "slog quoted secret fields with escapes and link keeps host/code",
			in:      `time=2026-11-15T10:00:00Z level=WARN msg="join failed" password="my \"quoted\" secret#1" link="bshare://join/7K3QX9MD#my secret" peer="alice#7F2A"`,
			want:    `time=2026-11-15T10:00:00Z level=WARN msg="join failed" password=[cut] link="bshare://join/7K3QX9MD#[cut]" peer="alice#7F2A"`,
			secrets: []string{"quoted", "secret#1", "my secret"},
		},
		{
			name:    "item 1: escaped newline and tab before link or code",
			in:      `err="dial failed\n\tbshare://join/K7PQ2M4X#pwNL2" detail="a\nK7PQ-2M4X#pwNL3" tab="a\tblite://join/ABCD#pwNL4" msg="line1\nhttps://h.example.com/j/ABCD#pw77\nline3"`,
			want:    `err="dial failed\n\tbshare://join/K7PQ2M4X#[cut]" detail="a\nK7PQ-2M4X#[cut]" tab="a\tblite://join/ABCD#[cut]" msg="line1\nhttps://h.example.com/j/ABCD#[cut]\nline3"`,
			secrets: []string{"pwNL2", "pwNL3", "pwNL4", "pw77"},
		},
		{
			name:    "item 3: bare code with spaces or dash before hash",
			in:      `joining k7pq-2m4x #pw14b` + "\n" + `joining   k7pq  2m4x  #pw14` + "\n" + `msg="paste k7pq 2m4x #pwSp5"`,
			want:    `joining k7pq-2m4x #[cut]` + "\n" + `joining   k7pq  2m4x  #[cut]` + "\n" + `msg="paste k7pq 2m4x #[cut]"`,
			secrets: []string{"pw14b", "pw14", "pwSp5"},
		},
		{
			name:    "item 4: passwords containing delimiters quotes brackets angles at and spaces in quoted query",
			in:      `u1=https://share.bellion.io/j/ABCD#pw'22' u2=https://share.bellion.io/j/ABCD#pw[25] u3=https://share.bellion.io/j/ABCD#pw<26> msg="x password=pw\"58 more" ws=wss://us%40er:p@ss31@host/ws url="https://h/x?token=a b41"`,
			want:    `u1=https://share.bellion.io/j/ABCD#[cut] u2=https://share.bellion.io/j/ABCD#[cut] u3=https://share.bellion.io/j/ABCD#[cut] msg="x password=[cut] more" ws=wss://[cut]@host/ws url="https://h/x?token=[cut]"`,
			secrets: []string{"pw'22'", "22", "pw[25]", "25", "pw<26>", "26", "pw\\", "58", "us%40er", "p@ss31", "ss31", "a b41", "b41"},
		},
		{
			name:    "item 5: colon space bearer plain cli flags and json numeric password",
			in:      `password: colonSecret1 auth="Authorization: Bearer bearerTok2" cmd="blite -headless -password cliPlain3 --api-key cliApi4 -view count" json={"password":123,"room":"ABCD"}`,
			want:    `password: [cut] auth="Authorization: Bearer [cut]" cmd="blite -headless -password [cut] --api-key [cut] -view count" json={"password":"[cut]","room":"ABCD"}`,
			secrets: []string{"colonSecret1", "bearerTok2", "cliPlain3", "cliApi4", "123"},
		},
		{
			name:    "items 6, 7, and H: preserve peer=alice#7F2A name=martin#0A1B chan#3 and small decimal/bool key while pass/secret/hex/6digit cut",
			in:      `time=2026-11-16T12:00:00Z level=INFO msg="state chan#3" peer=alice#7F2A name=martin#0A1B ch=chan#3 key=true key=false key=12 key=-42 key=99999 key=100000 key=0xDEADBEEF key=Inf key=NaN key=1_000 pass=false pass=482913 secret=0 token=12345 json={"key":12,"key2":"1234","pass":true,"secret":false,"password":4821}`,
			want:    `time=2026-11-16T12:00:00Z level=INFO msg="state chan#3" peer=alice#7F2A name=martin#0A1B ch=chan#3 key=true key=false key=12 key=-42 key=99999 key=[cut] key=[cut] key=[cut] key=[cut] key=[cut] pass=[cut] pass=[cut] secret=[cut] token=[cut] json={"key":12,"key2":"1234","pass":"[cut]","secret":"[cut]","password":"[cut]"}`,
			secrets: []string{"100000", "0xDEADBEEF", "DEADBEEF", "Inf", "NaN", "1_000", "482913", "12345", "4821"},
		},
		{
			name:    "escaped quote inside slog-quoted non-secret attribute does not cut early",
			in:      `time=2026-11-15T10:00:00Z level=WARN msg="bad link bshare://join/7K3QX9MD#first\"second\"third" raw="https://share.bellion.io/r/7K3QX9MD#my \"quoted\" tail" code="K7PQ-2M4X#pw \"escaped\" end"`,
			want:    `time=2026-11-15T10:00:00Z level=WARN msg="bad link bshare://join/7K3QX9MD#[cut]" raw="https://share.bellion.io/r/7K3QX9MD#[cut]" code="K7PQ-2M4X#[cut]"`,
			secrets: []string{"first", "second", "third", "quoted", "tail", "escaped", "end"},
		},
		{
			name:    "slog json secret fields case insensitive and URL in err",
			in:      `{"time":"2026-11-15T10:00:00Z","level":"ERROR","msg":"join","Password":"json\"Secret","roomPassword":"rp1","room_password":"rp2","pass":"p1","key":"k1","SECRET":"s1","api_key":"ak1","access_token":"at1","x-token":"xt1","passwd":"pwd1","Token":"jsonTok","link":"https://share.bellion.io/j/9999AAAA#jsonLinkSecret","err":"parse \"https://share.bellion.io/custom/7K3QX9MD#url\\\"Secret\": bad host"}`,
			want:    `{"time":"2026-11-15T10:00:00Z","level":"ERROR","msg":"join","Password":"[cut]","roomPassword":"[cut]","room_password":"[cut]","pass":"[cut]","key":"[cut]","SECRET":"[cut]","api_key":"[cut]","access_token":"[cut]","x-token":"[cut]","passwd":"[cut]","Token":"[cut]","link":"https://share.bellion.io/j/9999AAAA#[cut]","err":"parse \"https://share.bellion.io/custom/7K3QX9MD#[cut]\": bad host"}`,
			secrets: []string{"Secret", "rp1", "rp2", "k1", "s1", "ak1", "at1", "xt1", "pwd1", "jsonTok", "jsonLinkSecret"},
		},
		{
			name:    "go %+v and %#v struct output",
			in:      `msg="join" req={T:join Room:7K3QX9MD Password:structPw1 Token:structTok2} cfg={RoomPassword:rp3 room_password:rp4 Pass:p5 Key:k6 Secret:"quoted \"sec\" 7" NumKey:12 Key:12 Pass:true} bare=Password:solo8`,
			want:    `msg="join" req={T:join Room:7K3QX9MD Password:[cut] Token:[cut]} cfg={RoomPassword:[cut] room_password:[cut] Pass:[cut] Key:[cut] Secret:[cut] NumKey:12 Key:12 Pass:[cut]} bare=Password:[cut]`,
			secrets: []string{"structPw1", "structTok2", "rp3", "rp4", "p5", "k6", "sec", "solo8"},
		},
		{
			name:    "round 2 A: secret key right after opening quote",
			in:      `err="password=hunter2" msg="password=qS6" json={"msg":"password=qS7"} detail="token=qS8 x" e1="PASSWORD=upA1" e2="Pw=pwA2" e3="api_key=akA3" e4="token=12345"`,
			want:    `err="password=[cut]" msg="password=[cut]" json={"msg":"password=[cut]"} detail="token=[cut] x" e1="PASSWORD=[cut]" e2="Pw=[cut]" e3="api_key=[cut]" e4="token=[cut]"`,
			secrets: []string{"hunter2", "qS6", "qS7", "qS8", "upA1", "pwA2", "akA3", "12345"},
		},
		{
			name:    "round 2 B: double and triple escapes before link or code",
			in:      `err="bad link \"a\\nhttps://share.bellion.io/j/ABCD#pwQQ\"" m1="err=\"dial failed\\n\\tbshare://join/K7PQ2M4X#pwNL2\"" m2="detail=\"a\\nK7PQ-2M4X#pwNL3\"" m3="tab=\"a\\tblite://join/ABCD#pwNL4\"" m4="msg=\"line1\\nhttps://h.example.com/j/ABCD#pw77\\nline3\""`,
			want:    `err="bad link \"a\\nhttps://share.bellion.io/j/ABCD#[cut]\"" m1="err=\"dial failed\\n\\tbshare://join/K7PQ2M4X#[cut]\"" m2="detail=\"a\\nK7PQ-2M4X#[cut]\"" m3="tab=\"a\\tblite://join/ABCD#[cut]\"" m4="msg=\"line1\\nhttps://h.example.com/j/ABCD#[cut]\\nline3\""`,
			secrets: []string{"pwQQ", "pwNL2", "pwNL3", "pwNL4", "pw77"},
		},
		{
			name:    "round 2 C: %+v struct with link field and colon-prefixed link/code/see",
			in:      `{Room:K7PQ-2M4X Link:https://share.bellion.io/j/K7PQ2M4X#pwState} link:K7PQ2M4X#pwLinkC code:K7PQ2M4X#pwCodeC see:https://h/j/ABCD#pwSeeC`,
			want:    `{Room:K7PQ-2M4X Link:https://share.bellion.io/j/K7PQ2M4X#[cut]} link:K7PQ2M4X#[cut] code:K7PQ2M4X#[cut] see:https://h/j/ABCD#[cut]`,
			secrets: []string{"pwState", "pwLinkC", "pwCodeC", "pwSeeC"},
		},
		{
			name: "round 2 D: unicode and control bytes and hex/unicode escapes before link or code",
			in: "x\u00a0https://h.example.com/j/ABCD#pwJ1 " +
				"y\u200bhttps://h.example.com/j/ABCD#pwJ2 " +
				"z\u2028K7PQ-2M4X#pwJ3 " +
				"v\vhttps://h.example.com/j/ABCD#pwJ4 " +
				"f\fK7PQ2M4X#pwJ5 " +
				"n\x00https://h.example.com/j/ABCD#pwJ6 " +
				`lit1="x\u000ahttps://h.example.com/j/ABCD#pwJ7" lit2="x\x0aK7PQ-2M4X#pwJ8"`,
			want: "x\u00a0https://h.example.com/j/ABCD#[cut] " +
				"y\u200bhttps://h.example.com/j/ABCD#[cut] " +
				"z\u2028K7PQ-2M4X#[cut] " +
				"v\vhttps://h.example.com/j/ABCD#[cut] " +
				"f\fK7PQ2M4X#[cut] " +
				"n\x00https://h.example.com/j/ABCD#[cut] " +
				`lit1="x\u000ahttps://h.example.com/j/ABCD#[cut]" lit2="x\x0aK7PQ-2M4X#[cut]"`,
			secrets: []string{"pwJ1", "pwJ2", "pwJ3", "pwJ4", "pwJ5", "pwJ6", "pwJ7", "pwJ8"},
		},
		{
			name:    "round 2 F and Also: escaped json number, spaces around =/:, suffixed/dotted keys, raw unquoted quote in password",
			in:      `{"msg":"cfg {\"password\":4821}"} password = spEq1 password :spCol2 joinPassword=jp3 passwords=pws4 room.password=rpw5 password=pw"58 tail=ok`,
			want:    `{"msg":"cfg {\"password\":\"[cut]\"}"} password = [cut] password :[cut] joinPassword=[cut] passwords=[cut] room.password=[cut] password=[cut] tail=ok`,
			secrets: []string{"4821", "spEq1", "spCol2", "jp3", "pws4", "rpw5", `pw"58`, "58"},
		},
		{
			name:    "bare CODE#pw forms accepted by ParseJoinInput",
			in:      `msg="inputs: 7K3QX9MD#pwPlain K7PQ-2M4X#pwDash k7pq 2m4x#pwSpace MAMACO#pwHosted GAME#pwShort ABCDEFGHJKMN#pwLong (K7PQ-2M4X#pw/j/bar)" padded="  k7pq 2m4x  #pwPadded"`,
			want:    `msg="inputs: 7K3QX9MD#[cut] K7PQ-2M4X#[cut] k7pq 2m4x#[cut] MAMACO#[cut] GAME#[cut] ABCDEFGHJKMN#[cut] (K7PQ-2M4X#[cut])" padded="  k7pq 2m4x  #[cut]"`,
			secrets: []string{"pwPlain", "pwDash", "pwSpace", "pwHosted", "pwShort", "pwLong", "pw/j/bar", "pwPadded"},
		},
		{
			name:    "percent-encoded %23 fragments and share links on any path",
			in:      `msg="links https://share.bellion.io/room/7K3QX9MD#anyPath1 https://share.bellion.io/Invite/ABCD%23pctFrag2 share.bellion.io/s/MAMACO#anySchemeLess3 bshare://join/7K3QX9MD?server=s.example.com%23pctApp4 K7PQ-2M4X%23pctBare5"`,
			want:    `msg="links https://share.bellion.io/room/7K3QX9MD#[cut] https://share.bellion.io/Invite/ABCD#[cut] share.bellion.io/s/MAMACO#[cut] bshare://join/7K3QX9MD?server=s.example.com#[cut] K7PQ-2M4X#[cut]"`,
			secrets: []string{"anyPath1", "pctFrag2", "anySchemeLess3", "pctApp4", "pctBare5"},
		},
		{
			name:    "url userinfo and secret query parameters",
			in:      `err="dial wss://alice:uPass1@share.example.com:8443/ws?token=qTok2&password=qPw3&Key=qKey4&api_key=qAk5&access_token=qAt6&x-token=qXt7&room=7K3QX9MD: refused" alt=http://bearerOnly5@127.0.0.1:8080/p?secret=qSec6&pass=qPass7 bare=admin:bPass8@share.example.com/ws`,
			want:    `err="dial wss://[cut]@share.example.com:8443/ws?token=[cut]&password=[cut]&Key=[cut]&api_key=[cut]&access_token=[cut]&x-token=[cut]&room=7K3QX9MD: refused" alt=http://[cut]@127.0.0.1:8080/p?secret=[cut]&pass=[cut] bare=[cut]@share.example.com/ws`,
			secrets: []string{"uPass1", "qTok2", "qPw3", "qKey4", "qAk5", "qAt6", "qXt7", "bearerOnly5", "qSec6", "qPass7", "bPass8"},
		},
		{
			name:    "urls in prose messages and display name with tag preserved",
			in:      `msg="alice#7F2A opened bshare://join/K7PQ2M4X#pwOne and http://127.0.0.1:8080/j/MAMACO#pwTwo and share.bellion.io/j/ABCD1234#pwThree (blite://join/ZZZZ9999#pwFour)"`,
			want:    `msg="alice#7F2A opened bshare://join/K7PQ2M4X#[cut] and http://127.0.0.1:8080/j/MAMACO#[cut] and share.bellion.io/j/ABCD1234#[cut] (blite://join/ZZZZ9999#[cut])"`,
			secrets: []string{"pwOne", "pwTwo", "pwThree", "pwFour"},
		},
		{
			name: "bshare url without fragment and non-code name#tag stay intact",
			in:   `msg="bshare://join/K7PQ2M4X?server=share.example.com joined by bob#9A1C and paul#7F2A in #general"`,
			want: `msg="bshare://join/K7PQ2M4X?server=share.example.com joined by bob#9A1C and paul#7F2A in #general"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Text(tc.in)
			if got != tc.want {
				t.Fatalf("Text:\n got: %s\nwant: %s", got, tc.want)
			}
			for _, sec := range tc.secrets {
				if strings.Contains(got, sec) {
					t.Fatalf("secret %q survived in %s", sec, got)
				}
			}
			if twice := Text(got); twice != got {
				t.Fatalf("not idempotent:\nonce:  %s\ntwice: %s", got, twice)
			}
		})
	}
}

// Items 2 and E: secrets split across maxLineBuf (64 KiB) — unquoted, quoted
// (`pw="secret part one…"`), and JSON (`"password":"pw Js Tr …"`) — must never
// leak partial secret bytes across a forced flush.
func TestWriterGiantLineSplitAtMaxLineBuf(t *testing.T) {
	writeChunks := func(input string) string {
		var out bytes.Buffer
		rw := NewWriter(&out)
		const chunk = 4096
		for i := 0; i < len(input); i += chunk {
			end := min(i+chunk, len(input))
			n, err := rw.Write([]byte(input[i:end]))
			if err != nil || n != end-i {
				t.Fatalf("Write(%d..%d) = %d, %v", i, end, n, err)
			}
		}
		if err := rw.Flush(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	// 1. Round 1 Item 2: unquoted secret split at maxLineBuf.
	got1 := writeChunks(strings.Repeat("a", 65533) + " password=pwLong6\n" +
		"next line ok room=7K3QX9MD password=pwAfterTrunc\n")
	for _, sec := range []string{"pwLong6", "pwAfterTrunc"} {
		if strings.Contains(got1, sec) {
			t.Fatalf("Writer leaked %q across maxLineBuf boundary:\n%s", sec, got1[max(0, len(got1)-256):])
		}
	}
	if !strings.Contains(got1, "[line truncated]") {
		t.Fatalf("expected [line truncated] marker in output")
	}
	if !strings.Contains(got1, "next line ok room=7K3QX9MD password=[cut]\n") {
		t.Fatalf("subsequent line after truncated line missing or malformed:\n%s", got1[max(0, len(got1)-256):])
	}

	// 2. Round 2 Item E: quoted value `65520×a + ' pw="secret part one…"'`
	// drops back to `pw=[cut] [line truncated]`.
	got2 := writeChunks(strings.Repeat("a", 65520) + ` pw="secret part one two three"` + "\n" +
		"after quoted trunc password=pwAfter2\n")
	for _, sec := range []string{"secret", "part", "one", "two", "three", "pwAfter2"} {
		if strings.Contains(got2, sec) {
			t.Fatalf("Writer leaked %q in truncated quoted value:\n%s", sec, got2[max(0, len(got2)-256):])
		}
	}
	if !strings.Contains(got2, " pw=[cut] [line truncated]\n") {
		t.Fatalf("expected ' pw=[cut] [line truncated]' in output, got tail:\n%s", got2[max(0, len(got2)-120):])
	}
	if !strings.Contains(got2, "after quoted trunc password=[cut]\n") {
		t.Fatalf("missing subsequent line after quoted truncation")
	}

	// 3. Round 2 Item E: JSON `"password":"pw Js Tr …"` split at maxLineBuf.
	jsonPrefix := `{"pad":"` + strings.Repeat("a", 65508) + `","password":"pw Js Tr tail secret"}` + "\n"
	got3 := writeChunks(jsonPrefix)
	for _, sec := range []string{"pw Js", "Js", "Tr", "tail", "secret"} {
		if strings.Contains(got3, sec) {
			t.Fatalf("Writer leaked %q in truncated JSON value:\n%s", sec, got3[max(0, len(got3)-256):])
		}
	}
	if !strings.Contains(got3, `"password":"[cut]" [line truncated]`+"\n") {
		t.Fatalf("expected JSON truncated [cut] marker, got tail:\n%s", got3[max(0, len(got3)-120):])
	}
}

func TestWriterAndReplaceAttrOnDisk(t *testing.T) {
	type sampleState struct {
		Room string
		Link string
	}
	for _, jsonMode := range []bool{false, true} {
		name := "text"
		if jsonMode {
			name = "json"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "blite-disk.log")
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			rw := NewWriter(f)
			opts := &slog.HandlerOptions{ReplaceAttr: ReplaceAttr}
			var h slog.Handler = slog.NewTextHandler(rw, opts)
			if jsonMode {
				h = slog.NewJSONHandler(rw, opts)
			}
			logger := slog.New(h)

			logger.Info("auth",
				"Link", "https://share.bellion.io/r/7K3QX9MD?token=linkQTok0#linkFrag0",
				"RoomPassword", "rpDisk1",
				"room_password", "rpDisk2",
				"joinPassword", "jpDisk17",
				"passwords", "pwsDisk18",
				"room.password", "rpwDisk19",
				"Pass", "passDisk3",
				"KEY", "keyDisk4",
				"Secret", "secDisk5",
				"api_key", "akDisk13",
				"access_token", "atDisk14",
				"x-token", "xtDisk15",
				"passwd", "pwdDisk16",
				"PASSWORD", "upPwDisk20",
				"Pw", "mixedPwDisk21",
				"token", 12345,
				"key", true,
				"keySmall", 42,
				"pass", 482913,
				"peer", "alice#7F2A",
				"name", "martin#0A1B",
				"ch", "chan#3",
				"raw", `bshare://join/7K3QX9MD#my "quoted" pwDisk6`,
			)
			// Round 2 A: secret key right after opening quote via logger.
			logger.Warn("x", "err", errors.New("password=hunter2"))
			logger.Warn("x", "err", errors.New("PASSWORD=errUpPw22"), "e2", errors.New("Pw=errMixedPw23"), "e3", errors.New("api_key=errAk24"), "e4", errors.New("token=54321"))
			logger.Info(`msg="password=qS6"`)
			logger.Info(`{"msg":"password=qS7"}`)
			logger.Info(`detail="token=qS8 x"`)

			// Round 2 B: double escapes + every Round-1 Item-1 line logged as msg.
			logger.Warn("x", "err", fmt.Errorf("bad link %q", "a\nhttps://share.bellion.io/j/ABCD#pwQQ"))
			logger.Info("join failed:\nhttps://share.bellion.io/j/ABCD#pwNL1")
			logger.Info(`err="dial failed\n\tbshare://join/K7PQ2M4X#pwNL2"`)
			logger.Info(`detail="a\nK7PQ-2M4X#pwNL3"`)
			logger.Info(`tab="a\tblite://join/ABCD#pwNL4"`)
			logger.Info(`msg="line1\nhttps://h.example.com/j/ABCD#pw77\nline3"`)

			// Round 2 C: %+v struct with Link field and colon forms.
			st := sampleState{Room: "K7PQ-2M4X", Link: "https://share.bellion.io/j/K7PQ2M4X#pwState"}
			logger.Info("state", "stStr", fmt.Sprintf("%+v", st), "stVal", st, "more", "link:K7PQ2M4X#pwLinkC code:K7PQ2M4X#pwCodeC see:https://h/j/ABCD#pwSeeC")

			// Round 2 D: unicode / control bytes / literal escape text before link or code.
			logger.Info("u1 x\u00a0https://h.example.com/j/ABCD#pwJ1")
			logger.Info("u2 x\u200bhttps://h.example.com/j/ABCD#pwJ2")
			logger.Info("u3 x\u2028K7PQ-2M4X#pwJ3")
			logger.Info("u4 x\vhttps://h.example.com/j/ABCD#pwJ4")
			logger.Info("u5 x\fK7PQ2M4X#pwJ5")
			logger.Info("u6 x\x00https://h.example.com/j/ABCD#pwJ6")
			logger.Info(`u7 x\u000ahttps://h.example.com/j/ABCD#pwJ7 x\x0aK7PQ-2M4X#pwJ8`)

			// Round 2 F, H, Also:
			logger.Info(`cfg {"password":4821}`)
			logger.Info(`pass=482913 key=0xDEADBEEF key=Inf key=NaN key=1_000 key=100000 Pass:true`)
			logger.Info(`password = spEq25 password :spCol26 joinPassword=jp27 passwords=pws28 room.password=rpw29 password=pw"58 end`)

			// Simulate pipe reads splitting a line right through the middle of secrets.
			stream := "2026-11-16T10:00:00Z child: dial wss://us%40er:p@ssDisk7@share.example.com/ws?token=tDisk8&key=kDisk9\n" +
				"2026-11-16T10:00:01Z child: join K7PQ-2M4X#bareDisk10 and https://share.example.com/open/ABCD%23pctDisk11 struct={Password:stDisk12}\n"
			for i := 0; i < len(stream); i += 7 {
				end := min(i+7, len(stream))
				if _, err := rw.Write([]byte(stream[i:end])); err != nil {
					t.Fatal(err)
				}
			}
			if err := rw.Flush(); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}

			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			disk := string(b)
			// Timestamps carry nanoseconds, which can contain a numeric
			// secret like 4821 by chance: blank them for the leak scan.
			scan := timestampRE.ReplaceAllString(disk, "<time>")
			for _, sec := range []string{
				"linkQTok0", "linkFrag0",
				"rpDisk1", "rpDisk2", "jpDisk17", "pwsDisk18", "rpwDisk19",
				"passDisk3", "keyDisk4", "secDisk5", "pwDisk6",
				"ssDisk7", "tDisk8", "kDisk9", "bareDisk10", "pctDisk11", "stDisk12",
				"akDisk13", "atDisk14", "xtDisk15", "pwdDisk16",
				"upPwDisk20", "mixedPwDisk21", "12345", "482913",
				"hunter2", "errUpPw22", "errMixedPw23", "errAk24", "54321", "qS6", "qS7", "qS8",
				"pwQQ", "pwNL1", "pwNL2", "pwNL3", "pwNL4", "pw77",
				"pwState", "pwLinkC", "pwCodeC", "pwSeeC",
				"pwJ1", "pwJ2", "pwJ3", "pwJ4", "pwJ5", "pwJ6", "pwJ7", "pwJ8",
				"4821", "0xDEADBEEF", "DEADBEEF", "1_000", "100000",
				"spEq25", "spCol26", "jp27", "pws28", "rpw29", `pw"58`, `pw\"58`,
			} {
				if strings.Contains(scan, sec) {
					t.Fatalf("on-disk %s log leaked secret %q:\n%s", name, sec, disk)
				}
			}
			for _, keep := range []string{
				"https://share.bellion.io/r/7K3QX9MD?token=[cut]#[cut]",
				"alice#7F2A",
				"martin#0A1B",
				"chan#3",
				"https://share.bellion.io/j/K7PQ2M4X#[cut]",
				"https://h/j/ABCD#[cut]",
				"wss://[cut]@share.example.com/ws?token=[cut]&key=[cut]",
				"K7PQ-2M4X#[cut]",
				"https://share.example.com/open/ABCD#[cut]",
			} {
				if !strings.Contains(disk, keep) {
					t.Fatalf("on-disk %s log lost expected content %q:\n%s", name, keep, disk)
				}
			}
			if !jsonMode {
				for _, keep := range []string{"key=true", "pass=[cut]", `err="password=[cut]"`} {
					if !strings.Contains(disk, keep) {
						t.Fatalf("on-disk text log missing %q:\n%s", keep, disk)
					}
				}
			} else {
				for _, keep := range []string{`"key":true`, `"pass":"[cut]"`, `"err":"password=[cut]"`} {
					if !strings.Contains(disk, keep) {
						t.Fatalf("on-disk json log missing %q:\n%s", keep, disk)
					}
				}
			}
		})
	}
}

const benchPlainLine = `time=2026-11-16T12:00:00.123Z level=INFO msg="ice pair" version=0.1.0 peer=p9f2a state=succeeded local=192.168.1.5:30471 rttMs=18` + "\n"

func BenchmarkTextPlainLine(b *testing.B) {
	if len(benchPlainLine) < 120 {
		b.Fatalf("bench line too short: %d", len(benchPlainLine))
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(benchPlainLine)))
	var sink string
	for b.Loop() {
		sink = Text(benchPlainLine)
	}
	_ = sink
}

func BenchmarkTextWithSecrets(b *testing.B) {
	line := `time=2026-11-16T12:00:00Z level=WARN msg="join failed:\nhttps://share.bellion.io/j/ABCD#pwNL1" password=sec1 peer=alice#7F2A` + "\n"
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	var sink string
	for b.Loop() {
		sink = Text(line)
	}
	_ = sink
}

func repeat64KiB(unit string) string {
	return strings.Repeat(unit, maxLineBuf/len(unit))
}

func TestWorstCase64KiBLinear(t *testing.T) {
	hashLine := repeat64KiB("x#1 ")
	if got := Text(hashLine); got != hashLine {
		t.Fatalf("x#1 repeated changed unexpectedly (len %d -> %d)", len(hashLine), len(got))
	}

	pwLine := repeat64KiB("pw=1 ")
	wantPw := strings.Repeat("pw=[cut] ", maxLineBuf/len("pw=1 "))
	if got := Text(pwLine); got != wantPw {
		t.Fatalf("pw=1 repeated mismatch (len %d, want %d)", len(got), len(wantPw))
	}

	const linkUnit = "https://share.bellion.io/j/K7PQ2M4X#pw1 "
	linksLine := repeat64KiB(linkUnit)
	wantLinks := strings.Repeat("https://share.bellion.io/j/K7PQ2M4X#[cut] ", maxLineBuf/len(linkUnit))
	if got := Text(linksLine); got != wantLinks {
		t.Fatalf("links repeated mismatch (len %d, want %d)", len(got), len(wantLinks))
	}

	const queryUnit = "?token=a&"
	queryLine := repeat64KiB(queryUnit)
	wantQuery := strings.Repeat("?token=[cut]&", maxLineBuf/len(queryUnit))
	if got := Text(queryLine); got != wantQuery {
		t.Fatalf("?token=a& repeated mismatch (len %d, want %d)", len(got), len(wantQuery))
	}

	// Quoted 64 KiB variants exercising outerQuote / escaped-quote tracking.
	quotedPw := `msg="` + strings.Repeat("pw=1 ", 13000) + `pw=last"`
	wantQuotedPw := `msg="` + strings.Repeat("pw=[cut] ", 13000) + `pw=[cut]"`
	if got := Text(quotedPw); got != wantQuotedPw {
		t.Fatalf("quoted pw=1 repeated mismatch")
	}
	if twice := Text(wantQuotedPw); twice != wantQuotedPw {
		t.Fatalf("quoted pw=1 repeated not idempotent")
	}
}

func BenchmarkText64KiBHash(b *testing.B) {
	line := repeat64KiB("x#1 ")
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	var sink string
	for b.Loop() {
		sink = Text(line)
	}
	_ = sink
}

func BenchmarkText64KiBPwEq(b *testing.B) {
	line := repeat64KiB("pw=1 ")
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	var sink string
	for b.Loop() {
		sink = Text(line)
	}
	_ = sink
}

func BenchmarkText64KiBLinks(b *testing.B) {
	line := repeat64KiB("https://share.bellion.io/j/K7PQ2M4X#pw1 ")
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	var sink string
	for b.Loop() {
		sink = Text(line)
	}
	_ = sink
}

func BenchmarkText64KiBQueryToken(b *testing.B) {
	line := repeat64KiB("?token=a&")
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	var sink string
	for b.Loop() {
		sink = Text(line)
	}
	_ = sink
}

// M14 review item 1: the share-URL scan must not stop at an earlier unmatched
// '#'/'%23' nor at the end of the previous fragment. Expected outputs are
// HEAD's (pre-M14) outputs.
func TestM14ShareLinkAfterUnmatchedMarker(t *testing.T) {
	cases := []struct{ in, want string }{
		{`http://%23x#secretpw`, `http://%23x#[cut]`},
		{`?next=%23share.bellion.io/j/K7PQ2M4X#secretpw`, `?next=%23share.bellion.io/j/K7PQ2M4X#[cut]`},
		{`?next=%2523share.bellion.io/j/K7PQ2M4X#secretpw`, `?next=%2523share.bellion.io/j/K7PQ2M4X#[cut]`},
		{`?next=%23share.bellion.io/j/K7PQ2M4X%23secretpw`, `?next=%23share.bellion.io/j/K7PQ2M4X#[cut]`},
		{`wss://## s3cret`, `wss://##[cut] s3cret`},
		{`wss://#x#1`, `wss://#x#[cut]`},
		{`http://##u:p@`, `http://##[cut]`},
		// Item 5, allowed changes that must stay.
		{`https://share.bellion.io/j/ABCD#pw\nmartin#0A1B`, `https://share.bellion.io/j/ABCD#[cut]\nmartin#0A1B`},
		{`alice#7F2A#7K3QX9MD`, `alice#7F2A#[cut]`},
		// ...but a real share link after the escape is still cut.
		{`https://h.example.com/j/ABCD#pw\nhttps://h.example.com/j/martin#0A1B`, `https://h.example.com/j/ABCD#[cut]\nhttps://h.example.com/j/martin#[cut]`},
	}
	for _, c := range cases {
		got := Text(c.in)
		if got != c.want {
			t.Errorf("Text(%q)\n got  %q\n want %q", c.in, got, c.want)
		}
		if twice := Text(got); twice != got {
			t.Errorf("Text not idempotent on %q: %q", got, twice)
		}
	}
	// The same lines repeated to 64 KiB must stay linear and keep cutting.
	line := repeat64KiB("?next=%23share.bellion.io/j/K7PQ2M4X#secretpw ")
	if got := Text(line); strings.Contains(got, "secretpw") {
		t.Fatalf("repeated share link leaked")
	}
}

// M14 review item 2: a raw '\r' inside a quoted secret value must not end the
// value when a giant line is truncated.
func TestM14TruncationWithCarriageReturn(t *testing.T) {
	writeChunks := func(input string) string {
		var out bytes.Buffer
		rw := NewWriter(&out)
		for i := 0; i < len(input); i += 4096 {
			end := min(i+4096, len(input))
			if _, err := rw.Write([]byte(input[i:end])); err != nil {
				t.Fatal(err)
			}
		}
		if err := rw.Flush(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	got := writeChunks(strings.Repeat("a", 65500) + ` pw="sec` + "\r" + `ret part one two three four five six seven"` + "\n")
	for _, sec := range []string{"sec", "ret", "part", "one", "two", "three", "seven"} {
		if strings.Contains(got, sec) {
			t.Fatalf("leaked %q: %q", sec, got[max(0, len(got)-120):])
		}
	}
	if !strings.HasSuffix(got, " pw=[cut] [line truncated]\n") {
		t.Fatalf("want ' pw=[cut] [line truncated]', got tail %q", got[max(0, len(got)-120):])
	}

	got = writeChunks(`{"pad":"` + strings.Repeat("a", 65508) + `","password":"pw Js` + "\r" + `Tr more stuff tail secret"}` + "\n")
	for _, sec := range []string{"pw Js", "Js", "Tr", "more", "stuff", "tail", "secret"} {
		if strings.Contains(got, sec) {
			t.Fatalf("leaked %q: %q", sec, got[max(0, len(got)-120):])
		}
	}
	if !strings.HasSuffix(got, `"password":"[cut]" [line truncated]`+"\n") {
		t.Fatalf("want JSON [cut] marker, got tail %q", got[max(0, len(got)-120):])
	}
}

// M14 review item 3: `us%40er:p@ss@` repeated used to be quadratic in
// redactBareUserinfo.
func TestM14RepeatedBareUserinfoLinear(t *testing.T) {
	line := repeat64KiB("us%40er:p@ss@")
	start := time.Now()
	got := Text(line)
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("Text took %v on 64 KiB of us%%40er:p@ss@", d)
	}
	if got != line {
		t.Fatalf("no host follows the last '@', so nothing should be cut")
	}
	// With a real host at the end, all userinfo before the last '@' is cut.
	withHost := line + "host.example.com/ws"
	if got := Text(withHost); got != "[cut]@host.example.com/ws" {
		t.Fatalf("got %q", got[:min(len(got), 80)])
	}
}

// M14 review item 4: these used to panic in redactSecretFields.
func TestM14NoPanicOnEmptyJSONValueFollowedByKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{`"password":""password":x`, `"password":"[cut]"password":"[cut]"`},
		{`"pw":""pw":1`, `"pw":"[cut]"pw":"[cut]"`},
		{`{"pw":"","pw":"s3"}`, `{"pw":"[cut]","pw":"[cut]"}`},
	}
	for _, c := range cases {
		got := redactText(c.in, scanPrefilter(c.in)) // unguarded: must not panic
		if got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// M14 review item 4: if a redaction pass ever panics, Text, URL (and so
// ReplaceAttr) and Writer return a marker, never the raw line, and do not
// crash the caller.
func TestM14RecoverSafetyNet(t *testing.T) {
	testPassHook = func(string) { panic("boom") }
	defer func() { testPassHook = nil }()

	const line = "join https://share.bellion.io/j/K7PQ2M4X#hunter2 password=hunter3\n"
	if got := Text(line); got != "[redaction failed]\n" {
		t.Fatalf("Text = %q", got)
	}
	if got := Text("password=hunter3"); got != "[redaction failed]" {
		t.Fatalf("Text = %q", got)
	}
	if got := Text("plain line"); got != "plain line" {
		t.Fatalf("prefiltered plain line = %q", got)
	}
	if got := URL("https://share.bellion.io/j/K7PQ2M4X#hunter2"); got != "[redaction failed]" {
		t.Fatalf("URL = %q", got)
	}
	a := ReplaceAttr(nil, slog.String("link", "https://share.bellion.io/j/K7PQ2M4X#hunter2"))
	if got := a.Value.String(); got != "[redaction failed]" {
		t.Fatalf("ReplaceAttr(link) = %q", got)
	}

	var out bytes.Buffer
	rw := NewWriter(&out)
	if _, err := rw.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}
	// Split line through the buffered path.
	if _, err := rw.Write([]byte("password=hun")); err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Write([]byte("ter4\nok\n")); err != nil {
		t.Fatal(err)
	}
	// Giant line through the truncation path.
	if _, err := rw.Write([]byte(strings.Repeat("a", maxLineBuf+10) + " password=hunter5\n")); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, sec := range []string{"hunter", "K7PQ2M4X"} {
		if strings.Contains(got, sec) {
			t.Fatalf("Writer leaked %q after a redaction panic: %q", sec, got[:min(len(got), 200)])
		}
	}
	if !strings.HasPrefix(got, "[redaction failed]\n[redaction failed]\nok\n") {
		t.Fatalf("Writer output = %q", got[:min(len(got), 200)])
	}
}

// M14 review item 3 plus the M14 targets and other adversarial repeats: every
// 64 KiB line must stay fast (the benchmarks below give exact timings).
func TestM14AdversarialRepeatsLinear(t *testing.T) {
	units := []string{
		"x#1 ", "pw=1 ", "https://share.bellion.io/j/K7PQ2M4X#pw1 ", "?token=a&", "us%40er:p@ss@",
		`"`, `\"`, `\\\"`, `\`, `\\`, "@", `password="`, `password="x" `, `"password":"`, `"pw":""`,
		`pw="a\" `, "#", "%23", "%2523", "a#", "/#", "a.co/#", "a@b.co/#", ":a", "[:", "a:b@",
		"u:p@h.co ", `\n`, `\u000a`, "http://#", "wss://#x#", "K7PQ2M4X#", "k7pq 2m4x #", "alice#7F2A#",
		"Bearer ", `"-password" "`, "{Password:", "'", "(http://h/j/A#p ", `https://h/j/ABCD#p\nmartin#0A1B`,
		"password=\"x\\\"", "password='", "\u00a0#", "name=a#", `password="password='`,
		strings.Repeat("[a", 150) + "/#", strings.Repeat("+a", 150) + "/#", strings.Repeat(":a", 150) + "/#",
		`x{a:\"@`, `a:\"a@b.c:`, `{a:\"a@b.c:`, `{Password:\"pw\":\"a@b.c:`,
	}
	for _, u := range units {
		for _, line := range []string{repeat64KiB(u), `msg="` + repeat64KiB(u), "pw=" + repeat64KiB(u)} {
			best := time.Hour
			for range 3 {
				start := time.Now()
				Text(line)
				best = min(best, time.Since(start))
			}
			// Generous bound so the race detector and loaded CI machines
			// pass; quadratic behaviour takes seconds here.
			if best > 250*time.Millisecond {
				t.Errorf("Text(%q repeated, %d bytes) took %v", u, len(line), best)
			}
		}
	}
}

func BenchmarkText64KiBBareUserinfo(b *testing.B) {
	line := repeat64KiB("us%40er:p@ss@")
	b.ReportAllocs()
	b.SetBytes(int64(len(line)))
	var sink string
	for b.Loop() {
		sink = Text(line)
	}
	_ = sink
}

// Further adversarial 64 KiB repeats (not in the M14 table).
func BenchmarkText64KiBAdversarial(b *testing.B) {
	for _, u := range []struct{ name, unit string }{
		{"Quote", `"`},
		{"EscapedQuote", `\"`},
		{"BackslashRun", `\\`},
		{"At", "@"},
		{"PasswordQuote", `password="`},
		{"Hash", "#"},
		{"Pct23", "%23"},
		{"NameHash", "a#"},
		{"LinkEscapeName", `https://h/j/ABCD#p\nmartin#0A1B`},
		{"BracketHost", strings.Repeat("[a", 150) + "/#"},
		{"ColonHost", strings.Repeat(":a", 150) + "/#"},
		{"PasswordAltQuote", `password="password='`},
	} {
		b.Run(u.name, func(b *testing.B) {
			line := repeat64KiB(u.unit)
			b.ReportAllocs()
			b.SetBytes(int64(len(line)))
			var sink string
			for b.Loop() {
				sink = Text(line)
			}
			_ = sink
		})
	}
}

func TestNormalizedCodeLenMatchesCrockford(t *testing.T) {
	for c := 0; c < 256; c++ {
		u := byte(c)
		if u >= 'a' && u <= 'z' {
			u -= 'a' - 'A'
		}
		switch u {
		case 'O':
			u = '0'
		case 'I', 'L':
			u = '1'
		}
		want := 1
		if c == '-' || c == ' ' {
			want = 0
		} else if strings.IndexByte(crockford, u) < 0 {
			want = -1
		}
		if got := normalizedCodeLen(string([]byte{byte(c)})); got != want {
			t.Errorf("normalizedCodeLen(%q) = %d, want %d", rune(c), got, want)
		}
	}
}

// Follow-up review: the user: authority memo must hit even when the quote
// state alternates between boundaries (repeated `\"`).
func BenchmarkText64KiBAuthorityQuoteState(b *testing.B) {
	for _, u := range []struct{ name, unit string }{
		{"BraceColonEscQuoteAt", `x{a:\"@`},
		{"ColonEscQuoteHost", `a:\"a@b.c:`},
		{"BraceColonEscQuoteHost", `{a:\"a@b.c:`},
		{"StructPasswordJSONHost", `{Password:\"pw\":\"a@b.c:`},
	} {
		b.Run(u.name, func(b *testing.B) {
			line := repeat64KiB(u.unit)
			b.ReportAllocs()
			b.SetBytes(int64(len(line)))
			var sink string
			for b.Loop() {
				sink = Text(line)
			}
			_ = sink
		})
	}
}

// A redaction failure on one line of a multi-line Write replaces only that line.
func TestWriterRecoverIsPerLine(t *testing.T) {
	testPassHook = func(s string) {
		if strings.Contains(s, "boom") {
			panic("boom")
		}
	}
	defer func() { testPassHook = nil }()
	var out bytes.Buffer
	rw := NewWriter(&out)
	in := "a password=s1\nboom password=s2\nc password=s3\n"
	if _, err := rw.Write([]byte(in)); err != nil {
		t.Fatal(err)
	}
	want := "a password=[cut]\n[redaction failed]\nc password=[cut]\n"
	if got := out.String(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// timestampRE matches the RFC 3339 times slog writes.
var timestampRE = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)`)
