package client

import "errors"

var (
	// ErrCouldNotCheck: the server was unreachable, returned an unexpected
	// status, or served an expired manifest. Apps should stay quiet about it.
	ErrCouldNotCheck = errors.New("molt: could not check for updates")
	// ErrBadSignature: the manifest failed verification or its signed identity
	// does not match the request. Treat as tampering.
	ErrBadSignature = errors.New("molt: manifest failed verification")
	// ErrBadPayload: the downloaded file does not match the signed sha256/size.
	ErrBadPayload = errors.New("molt: payload failed verification")
	// ErrNoRelease: no release published for this package/channel/platform.
	ErrNoRelease = errors.New("molt: no release published")
)
