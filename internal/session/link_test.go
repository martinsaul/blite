package session

import "testing"

func TestParseLink(t *testing.T) {
	for _, s := range []string{
		"https://share.example.com/j/7K3QX9MD#se%20cret",
		"bshare://join/7K3QX9MD#se%20cret",
		"blite://join/7K3QX9MD#se%20cret", // links shared before the rename
	} {
		code, pw, err := ParseLink(s)
		if err != nil || code != "7K3QX9MD" || pw != "se cret" {
			t.Errorf("%s: got %q %q %v", s, code, pw, err)
		}
	}
	for _, s := range []string{"ftp://join/7K3QX9MD#pw", "bshare://join/#pw"} { // a link without #password is an open room
		if _, _, err := ParseLink(s); err == nil {
			t.Errorf("%s: want an error", s)
		}
	}
}
