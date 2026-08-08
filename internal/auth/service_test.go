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

func TestValidSessionTokenRejectsMalformedValues(t *testing.T) {
	token, _, err := newSessionToken()
	if err != nil {
		t.Fatalf("generate session token: %v", err)
	}
	if !validSessionToken(token) {
		t.Fatal("generated session token was rejected")
	}

	for _, malformed := range []string{
		"",
		"wrong_prefix",
		sessionTokenPrefix + "not-base64!",
		sessionTokenPrefix + "c2hvcnQ",
	} {
		if validSessionToken(malformed) {
			t.Fatalf("malformed token %q was accepted", malformed)
		}
	}
}
