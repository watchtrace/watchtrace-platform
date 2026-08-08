package auth

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestNewSessionTokenUsesRandomOpaqueValueAndDigest(t *testing.T) {
	random := bytes.NewReader(bytes.Repeat([]byte{0x5a}, sessionTokenBytes))
	token, digest, err := newSessionTokenFrom(random)
	if err != nil {
		t.Fatalf("generate session token: %v", err)
	}

	if !strings.HasPrefix(token, sessionTokenPrefix) {
		t.Fatalf("token %q does not have the expected prefix", token)
	}
	encoded := strings.TrimPrefix(token, sessionTokenPrefix)
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode session token: %v", err)
	}
	if len(raw) != sessionTokenBytes {
		t.Fatalf("session token has %d random bytes, want %d", len(raw), sessionTokenBytes)
	}
	if bytes.Equal(digest, []byte(token)) {
		t.Fatal("session digest contains the raw token")
	}
	if !bytes.Equal(digest, sessionTokenDigest(token)) {
		t.Fatal("session digest is not reproducible")
	}
}

func TestNewSessionTokenPropagatesRandomnessFailure(t *testing.T) {
	_, _, err := newSessionTokenFrom(errorReader{})
	if err == nil {
		t.Fatal("session token generation succeeded with a failing random source")
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("randomness unavailable")
}
