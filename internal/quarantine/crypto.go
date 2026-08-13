// Package quarantine encrypts bounded queue payloads before operator storage.
package quarantine

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
)

const MaxPlaintextBytes = 64 * 1024

type Sealer struct{ aead cipher.AEAD }

func New(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, errors.New("quarantine key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

func (s *Sealer) Seal(plain, associated []byte) ([]byte, error) {
	if s == nil || len(plain) == 0 || len(plain) > MaxPlaintextBytes || len(associated) > 1024 {
		return nil, errors.New("invalid quarantine payload")
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plain, associated), nil
}

func (s *Sealer) Open(value, associated []byte) ([]byte, error) {
	if s == nil || len(value) <= s.aead.NonceSize() || len(value) > MaxPlaintextBytes+s.aead.NonceSize()+s.aead.Overhead() || len(associated) > 1024 {
		return nil, errors.New("invalid quarantine payload")
	}
	plain, err := s.aead.Open(nil, value[:s.aead.NonceSize()], value[s.aead.NonceSize():], associated)
	if err != nil {
		return nil, errors.New("quarantine decryption failed")
	}
	return plain, nil
}
