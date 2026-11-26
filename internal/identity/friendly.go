package identity

import (
	"crypto/rand"
	"math/big"
)

// A new user's display name until they choose one: two random words, like
// "Quiet Otter". It used to be the computer's name, which every member and
// the server saw and which often holds a person's real name.
var (
	friendlyAdj = []string{
		"Amber", "Brave", "Bright", "Calm", "Clever", "Cosy", "Curious", "Daring", "Eager", "Gentle",
		"Golden", "Happy", "Jolly", "Kind", "Lively", "Lucky", "Mellow", "Merry", "Nimble", "Patient",
		"Plucky", "Quick", "Quiet", "Rapid", "Sunny", "Swift", "Tidy", "Witty", "Silver", "Steady",
	}
	friendlyNoun = []string{
		"Badger", "Beaver", "Crane", "Falcon", "Ferret", "Finch", "Fox", "Gecko", "Hare", "Heron",
		"Koala", "Lark", "Lynx", "Marten", "Moose", "Newt", "Otter", "Owl", "Panda", "Puffin",
		"Raven", "Robin", "Seal", "Sparrow", "Stoat", "Swan", "Tiger", "Walrus", "Wombat", "Wren",
	}
)

// FriendlyName returns a random two-word display name.
func FriendlyName() string {
	return pick(friendlyAdj) + " " + pick(friendlyNoun)
}

func pick(words []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
	if err != nil {
		return words[0]
	}
	return words[n.Int64()]
}
