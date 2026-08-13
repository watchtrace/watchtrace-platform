// Package secureheaders encrypts monitor request headers before persistence.
package secureheaders

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const MaxPlaintextBytes = 8192

var ErrInvalid = errors.New("invalid monitor headers")

type Keyring struct {
	Current int32
	Keys    map[int32][]byte
}

func New(current int32, keys map[int32][]byte) (*Keyring, error) {
	if current < 1 || len(keys[current]) != 32 {
		return nil, errors.New("current header key must be 32 bytes")
	}
	copyKeys := make(map[int32][]byte, len(keys))
	for version, key := range keys {
		if version < 1 || len(key) != 32 {
			return nil, errors.New("header keys must be versioned AES-256 keys")
		}
		copyKeys[version] = append([]byte(nil), key...)
	}
	return &Keyring{Current: current, Keys: copyKeys}, nil
}

func (k *Keyring) Encrypt(headers map[string]string) ([]byte, int32, error) {
	normalized, err := Normalize(headers)
	if err != nil {
		return nil, 0, err
	}
	if len(normalized) == 0 {
		return nil, 0, nil
	}
	plain, _ := json.Marshal(normalized)
	if len(plain) > MaxPlaintextBytes {
		return nil, 0, ErrInvalid
	}
	aead, err := k.aead(k.Current)
	if err != nil {
		return nil, 0, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, 0, err
	}
	return aead.Seal(nonce, nonce, plain, nil), k.Current, nil
}

func (k *Keyring) Decrypt(data []byte, version int32) (map[string]string, error) {
	if len(data) == 0 {
		return map[string]string{}, nil
	}
	aead, err := k.aead(version)
	if err != nil || len(data) < aead.NonceSize() {
		return nil, errors.New("header decryption failed")
	}
	plain, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], nil)
	if err != nil || len(plain) > MaxPlaintextBytes {
		return nil, errors.New("header decryption failed")
	}
	var headers map[string]string
	if json.Unmarshal(plain, &headers) != nil {
		return nil, errors.New("header decryption failed")
	}
	return Normalize(headers)
}

func (k *Keyring) aead(version int32) (cipher.AEAD, error) {
	key, ok := k.Keys[version]
	if !ok {
		return nil, errors.New("header key version unavailable")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func Normalize(headers map[string]string) (map[string]string, error) {
	if len(headers) > 32 {
		return nil, ErrInvalid
	}
	out := make(map[string]string, len(headers))
	for name, value := range headers {
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		if name == "" || len(name) > 128 || len(value) > 2048 || strings.ContainsAny(name+value, "\r\n") ||
			name == "Host" || name == "Content-Length" || name == "Connection" || name == "Transfer-Encoding" {
			return nil, ErrInvalid
		}
		out[name] = value
	}
	return out, nil
}
