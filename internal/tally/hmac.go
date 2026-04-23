package tally

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

// HMAC request-signing for the Tally agent ingest path. The canonical string
// and header set here MUST match GST_Reco's src/lib/tally/hmac.ts exactly —
// any divergence means 401s from the server that look like key mismatches.
//
// Canonical string: METHOD\npath\ntimestamp\nnonce\nsha256(body)
// Headers (all required on every signed request):
//   authorization          Bearer <token>
//   x-tally-connection-id  <uuid>
//   x-tally-timestamp      <unix seconds>
//   x-tally-nonce          <16 random bytes hex, i.e. 32 chars>
//   x-tally-signature      <hex HMAC-SHA256 of canonical string>

const (
	HeaderConnectionID = "x-tally-connection-id"
	HeaderTimestamp    = "x-tally-timestamp"
	HeaderNonce        = "x-tally-nonce"
	HeaderSignature    = "x-tally-signature"
	HeaderAuth         = "Authorization"
)

// MaxSkewSeconds is the server's tolerance on x-tally-timestamp. Mirrors
// GST_Reco's MAX_SKEW_SECONDS (300). The installer runs `w32tm /resync` once
// to align the machine's clock with windows.com; beyond 5 minutes of drift
// something is seriously off.
const MaxSkewSeconds = 300

// NonceBytes is the raw-byte length of a nonce before hex-encoding. Must
// stay at 16 — the server's nonce table has a constraint on exact length.
const NonceBytes = 16

// CanonicalString builds the string to HMAC. Separated from Sign so tests
// can assert the canonical shape without a real secret. Body bytes are
// hashed (not included raw) so the signature fits in a header.
func CanonicalString(method, path, timestamp, nonce string, body []byte) string {
	sum := sha256.Sum256(body)
	return method + "\n" + path + "\n" + timestamp + "\n" + nonce + "\n" + hex.EncodeToString(sum[:])
}

// Sign returns the hex-encoded HMAC-SHA256 of the canonical string. Caller
// must put it in the `x-tally-signature` header.
func Sign(secret []byte, method, path, timestamp, nonce string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(CanonicalString(method, path, timestamp, nonce, body)))
	return hex.EncodeToString(mac.Sum(nil))
}

// NewNonce returns 16 random bytes hex-encoded. Safe for concurrent use —
// crypto/rand reads from the OS pool. Returned value is exactly 32 hex
// characters.
func NewNonce() (string, error) {
	buf := make([]byte, NonceBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("hmac: read nonce: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// NowTimestamp returns the current time formatted as unix seconds, the shape
// the server expects in x-tally-timestamp. Factored out so tests can use a
// frozen clock.
func NowTimestamp(now time.Time) string {
	return strconv.FormatInt(now.UTC().Unix(), 10)
}
