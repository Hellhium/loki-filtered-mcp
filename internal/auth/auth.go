// Package auth maps a client-presented bearer token to the value it grants
// access to. It is deliberately generic over that value so it has no dependency
// on the instance graph it indexes.
//
// Two properties matter here and are the reason this is not a plain map[string]T:
//
//   - Tokens are indexed by their SHA-256, so the index never holds a plaintext
//     credential in memory as a map key, and lookup cost does not vary with how
//     much of a token an attacker has guessed correctly.
//   - A hash hit is confirmed with a constant-time comparison of the full token
//     before the value is returned, so a (theoretical) hash collision cannot
//     authenticate.
//
// Lookup failures are indistinguishable to the caller: there is one boolean, no
// error variants, and nothing that names an instance.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
)

// Entry declares one set of tokens and the value they grant access to.
type Entry[T any] struct {
	// Name identifies the entry in construction errors. It is never returned
	// by Lookup and never derived from a token.
	Name   string
	Tokens []string
	Value  T
}

type indexed[T any] struct {
	token string
	name  string
	value T
}

// Index resolves bearer tokens to values. It is immutable after construction
// and safe for concurrent use.
type Index[T any] struct {
	byHash map[[sha256.Size]byte]indexed[T]
}

// NewIndex builds the token index. It fails if a token is empty or is claimed
// by two entries — an ambiguous token would mean an ambiguous scope.
func NewIndex[T any](entries []Entry[T]) (*Index[T], error) {
	ix := &Index[T]{byHash: make(map[[sha256.Size]byte]indexed[T])}
	for _, e := range entries {
		for _, tok := range e.Tokens {
			if tok == "" {
				return nil, fmt.Errorf("%s: empty auth token", e.Name)
			}
			h := sha256.Sum256([]byte(tok))
			if prev, ok := ix.byHash[h]; ok {
				return nil, fmt.Errorf("%s: auth token is also used by %q", e.Name, prev.name)
			}
			ix.byHash[h] = indexed[T]{token: tok, name: e.Name, value: e.Value}
		}
	}
	return ix, nil
}

// Len returns the number of indexed tokens.
func (ix *Index[T]) Len() int { return len(ix.byHash) }

// Lookup returns the value the token grants access to. The second result is
// false for an unknown token, and the zero value of T is returned.
func (ix *Index[T]) Lookup(token string) (T, bool) {
	var zero T
	if token == "" {
		return zero, false
	}
	got, ok := ix.byHash[sha256.Sum256([]byte(token))]
	if !ok {
		return zero, false
	}
	// Confirm the full token: a hash hit alone must not authenticate.
	if subtle.ConstantTimeCompare([]byte(token), []byte(got.token)) != 1 {
		return zero, false
	}
	return got.value, true
}

// bearerPrefix is matched case-insensitively, as RFC 7235 requires for the
// auth-scheme token.
const bearerPrefix = "bearer "

// BearerToken extracts the credential from an Authorization: Bearer header.
// It returns false when the header is absent, uses another scheme, or carries
// an empty credential — the caller must treat all three identically.
func BearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < len(bearerPrefix) || !strings.EqualFold(h[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(bearerPrefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}
