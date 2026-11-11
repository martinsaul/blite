// Package register makes the OS open bshare:// (and older blite://) links
// with this executable.
package register

// Scheme is the URL scheme share links hand off to. It is neutral on
// purpose: the product name may change again, links shouldn't.
const Scheme = "bshare"

// Schemes are all the schemes we handle: blite:// stays registered because
// the hosted join page and links shared before the rename still use it.
var Schemes = []string{Scheme, "blite"}
