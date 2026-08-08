package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

const (
	argonMemory      = 64 * 1024
	argonIterations  = 3
	argonParallelism = 2
	argonSaltLength  = 16
	argonKeyLength   = 32
)

var (
	errInvalidPasswordHash = errors.New("invalid password hash")
	dummyHashOnce          sync.Once
	dummyHashValue         string
)

func hashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}

	return encodePassword(password, salt), nil
}

func verifyPassword(password, encoded string) bool {
	salt, expected, err := parsePasswordHash(encoded)
	if err != nil {
		return false
	}

	actual := argon2.IDKey(
		[]byte(password),
		salt,
		argonIterations,
		argonMemory,
		argonParallelism,
		argonKeyLength,
	)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func dummyPasswordHash() string {
	dummyHashOnce.Do(func() {
		dummyHashValue = encodePassword(
			"watchtrace-dummy-password",
			[]byte("fixed-dummy-salt"),
		)
	})
	return dummyHashValue
}

func encodePassword(password string, salt []byte) string {
	key := argon2.IDKey(
		[]byte(password),
		salt,
		argonIterations,
		argonMemory,
		argonParallelism,
		argonKeyLength,
	)

	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemory,
		argonIterations,
		argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

func parsePasswordHash(encoded string) ([]byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return nil, nil, errInvalidPasswordHash
	}

	if !strings.HasPrefix(parts[2], "v=") {
		return nil, nil, errInvalidPasswordHash
	}
	version, err := strconv.Atoi(strings.TrimPrefix(parts[2], "v="))
	if err != nil || version != argon2.Version {
		return nil, nil, errInvalidPasswordHash
	}

	parameters := strings.Split(parts[3], ",")
	if len(parameters) != 3 {
		return nil, nil, errInvalidPasswordHash
	}
	memory, err := parseHashParameter(parameters[0], "m=")
	if err != nil || memory != argonMemory {
		return nil, nil, errInvalidPasswordHash
	}
	iterations, err := parseHashParameter(parameters[1], "t=")
	if err != nil || iterations != argonIterations {
		return nil, nil, errInvalidPasswordHash
	}
	parallelism, err := parseHashParameter(parameters[2], "p=")
	if err != nil || parallelism != argonParallelism {
		return nil, nil, errInvalidPasswordHash
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) != argonSaltLength {
		return nil, nil, errInvalidPasswordHash
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(key) != argonKeyLength {
		return nil, nil, errInvalidPasswordHash
	}

	return salt, key, nil
}

func parseHashParameter(value, prefix string) (uint64, error) {
	if !strings.HasPrefix(value, prefix) || len(value) == len(prefix) {
		return 0, errInvalidPasswordHash
	}
	parsed, err := strconv.ParseUint(strings.TrimPrefix(value, prefix), 10, 32)
	if err != nil {
		return 0, errInvalidPasswordHash
	}
	return parsed, nil
}
