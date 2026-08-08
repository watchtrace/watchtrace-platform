package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
)

const (
	sessionTokenPrefix = "wt_local_"
	sessionTokenBytes  = 32
)

func newSessionToken() (string, []byte, error) {
	return newSessionTokenFrom(rand.Reader)
}

func newSessionTokenFrom(random io.Reader) (string, []byte, error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", nil, fmt.Errorf("generate session token: %w", err)
	}

	token := sessionTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return token, sessionTokenDigest(token), nil
}

func sessionTokenDigest(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}
