package auth

import (
	"strings"
	"testing"
)

func TestValidateCredentialsNormalizesEmailAndBoundsPassword(t *testing.T) {
	normalized, err := validateCredentials(" User@Example.Test ", "valid-test-password")
	if err != nil {
		t.Fatalf("validate credentials: %v", err)
	}
	if normalized != "user@example.test" {
		t.Fatalf("normalized email = %q, want user@example.test", normalized)
	}

	for _, test := range []struct {
		name     string
		email    string
		password string
	}{
		{name: "display name", email: "User <user@example.test>", password: "valid-test-password"},
		{name: "invalid email", email: "not-an-email", password: "valid-test-password"},
		{name: "short password", email: "user@example.test", password: "too-short"},
		{name: "oversized password", email: "user@example.test", password: strings.Repeat("p", maximumPasswordBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateCredentials(test.email, test.password); err != ErrInvalidInput {
				t.Fatalf("validation error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestValidAccessAndRefreshTokensRejectMalformedValues(t *testing.T) {
	token, _, err := newAccessToken()
	if err != nil {
		t.Fatalf("generate access token: %v", err)
	}
	if !validAccessToken(token) {
		t.Fatal("generated access token was rejected")
	}
	refreshToken, _, err := newRefreshToken()
	if err != nil {
		t.Fatalf("generate refresh token: %v", err)
	}
	if !validRefreshToken(refreshToken) || validAccessToken(refreshToken) {
		t.Fatal("generated refresh token was not isolated from access tokens")
	}

	for _, malformed := range []string{
		"",
		"wrong_prefix",
		accessTokenPrefix + "not-base64!",
		accessTokenPrefix + "c2hvcnQ",
	} {
		if validAccessToken(malformed) {
			t.Fatalf("malformed token %q was accepted", malformed)
		}
	}
}
