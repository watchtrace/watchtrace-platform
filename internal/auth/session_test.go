package auth

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestNewOpaqueTokensUseRandomValuesAndDigests(t *testing.T) {
	for _, prefix := range []string{accessTokenPrefix, refreshTokenPrefix} {
		t.Run(prefix, func(t *testing.T) {
			random := bytes.NewReader(bytes.Repeat([]byte{0x5a}, sessionTokenBytes))
			token, digest, err := newOpaqueTokenFrom(random, prefix)
			if err != nil {
				t.Fatalf("generate token: %v", err)
			}

			if !strings.HasPrefix(token, prefix) {
				t.Fatalf("token %q does not have the expected prefix", token)
			}
			encoded := strings.TrimPrefix(token, prefix)
			raw, err := base64.RawURLEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatalf("decode token: %v", err)
			}
			if len(raw) != sessionTokenBytes {
				t.Fatalf("token has %d random bytes, want %d", len(raw), sessionTokenBytes)
			}
			if bytes.Equal(digest, []byte(token)) {
				t.Fatal("digest contains the raw token")
			}
			if !bytes.Equal(digest, tokenDigest(token)) {
				t.Fatal("digest is not reproducible")
			}
		})
	}
}

func TestNewOpaqueTokenPropagatesRandomnessFailure(t *testing.T) {
	_, _, err := newOpaqueTokenFrom(errorReader{}, accessTokenPrefix)
	if err == nil {
		t.Fatal("session token generation succeeded with a failing random source")
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("randomness unavailable")
}
