package auth

import (
	"strings"
	"testing"
)

func TestPasswordHashUsesArgon2idAndUniqueSalt(t *testing.T) {
	const password = "correct horse battery staple"

	first, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	second, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hash password again: %v", err)
	}

	if first == second {
		t.Fatal("password hashes reused a salt")
	}
	if strings.Contains(first, password) {
		t.Fatal("password hash contains the plaintext password")
	}
	if !strings.HasPrefix(first, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("password hash has unexpected parameters: %q", first)
	}
	if !verifyPassword(password, first) {
		t.Fatal("correct password did not verify")
	}
	if verifyPassword("wrong password", first) {
		t.Fatal("wrong password verified")
	}
}

func TestPasswordVerificationRejectsMalformedHash(t *testing.T) {
	for _, encoded := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$a2V5",
		"$argon2id$19$m=65536,t=3,p=2$c2FsdA$a2V5",
		"$argon2id$v=19$m=1,t=1,p=1$c2FsdA$a2V5",
		"$argon2id$v=19$m=65536,t=3,p=2,trailing=1$c2FsdA$a2V5",
	} {
		if verifyPassword("password", encoded) {
			t.Fatalf("malformed hash %q verified", encoded)
		}
	}
}
