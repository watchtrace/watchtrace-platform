package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

const (
	accessTokenPrefix       = "wt_access_"
	legacyAccessTokenPrefix = "wt_local_"
	refreshTokenPrefix      = "wt_refresh_"
	sessionTokenBytes       = 32
)

func newAccessToken() (string, []byte, error) {
	return newOpaqueTokenFrom(rand.Reader, accessTokenPrefix)
}

func newRefreshToken() (string, []byte, error) {
	return newOpaqueTokenFrom(rand.Reader, refreshTokenPrefix)
}

func newOpaqueTokenFrom(random io.Reader, prefix string) (string, []byte, error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", nil, fmt.Errorf("generate session token: %w", err)
	}

	token := prefix + base64.RawURLEncoding.EncodeToString(raw)
	return token, tokenDigest(token), nil
}

func tokenDigest(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}

func validAccessToken(token string) bool {
	return validOpaqueToken(token, accessTokenPrefix) ||
		validOpaqueToken(token, legacyAccessTokenPrefix)
}

func validRefreshToken(token string) bool {
	return validOpaqueToken(token, refreshTokenPrefix)
}

func validOpaqueToken(token, prefix string) bool {
	if !strings.HasPrefix(token, prefix) {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(token, prefix))
	return err == nil && len(raw) == sessionTokenBytes
}
