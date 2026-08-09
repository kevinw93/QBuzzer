// Package secret hashes moderator passwords with argon2id and hashes session
// tokens for storage.
package secret

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are argon2id cost parameters.
type Params struct {
	Time    uint32
	Memory  uint32 // KiB
	Threads uint8
}

// Default follows OWASP's argon2id recommendation (19 MiB, 2 passes).
var Default = Params{Time: 2, Memory: 19 * 1024, Threads: 1}

// Fast is for tests only.
var Fast = Params{Time: 1, Memory: 64, Threads: 1}

const (
	saltLen = 16
	keyLen  = 32
)

// HashPassword returns a PHC-format argon2id hash with a random salt.
func HashPassword(password string, p Params) string {
	salt := make([]byte, saltLen)
	_, _ = rand.Read(salt) // never fails
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, keyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, enc.EncodeToString(salt), enc.EncodeToString(key))
}

var ErrMalformedHash = errors.New("secret: malformed password hash")

// VerifyPassword reports whether password matches a HashPassword result, in
// constant time with respect to the key. It costs as much as hashing, so
// callers must not run it on a latency-critical goroutine.
func VerifyPassword(password, phc string) (bool, error) {
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrMalformedHash
	}
	var version int
	var p Params
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrMalformedHash
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return false, ErrMalformedHash
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[4])
	want, err2 := enc.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false, ErrMalformedHash
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(len(want))) //nolint:gosec // len(want) is a decoded key length, far below 2^32
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// HashToken hashes a high-entropy session token for storage and lookup. Tokens
// are random 130-bit strings, so a fast hash is enough: there is nothing to
// brute-force.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
