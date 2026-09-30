// Package cursor encodes the opaque pagination token of the message lists.
//
// A token is base64.RawURLEncoding("v1.<unix microseconds>.<message id>"): the
// keyset position (created_at, id) of the last message of a page, so the next
// page does not depend on that message still existing. Tokens do not expire.
// A token carries no authorization and is not tamper-proof: it only moves the
// pagination boundary, and what rows are readable is still decided by the
// endpoint's thread or share-link conditions.
package cursor

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrInvalid is returned for a token this version cannot read: malformed,
// an unknown version, or a pre-v1 cursor (a bare message id). Callers match
// it with errors.Is and answer 400; the client restarts from the first page.
var ErrInvalid = errors.New("invalid pagination cursor")

const version = "v1"

// Position is a message's place in the (created_at, id) order.
type Position struct {
	CreatedAt time.Time
	ID        string
}

// Encode returns the token for p. CreatedAt is kept to the microsecond,
// the precision of Postgres timestamps.
func Encode(p Position) (string, error) {
	if p.ID == "" || strings.Contains(p.ID, ".") {
		return "", fmt.Errorf("cursor: message id %q cannot be encoded", p.ID)
	}
	raw := version + "." + strconv.FormatInt(p.CreatedAt.UnixMicro(), 10) + "." + p.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw)), nil
}

// Decode reads a token. An empty token is the first page and returns nil.
func Decode(token string) (*Position, error) {
	if token == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return nil, ErrInvalid
	}
	fields := strings.Split(string(raw), ".")
	if len(fields) != 3 || fields[0] != version || fields[2] == "" {
		return nil, ErrInvalid
	}
	micros, err := strconv.ParseInt(fields[1], 10, 64)
	// Only the canonical spelling: no "+", no leading zeros.
	if err != nil || strconv.FormatInt(micros, 10) != fields[1] {
		return nil, ErrInvalid
	}
	return &Position{CreatedAt: time.UnixMicro(micros).UTC(), ID: fields[2]}, nil
}
