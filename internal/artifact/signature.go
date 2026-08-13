// Package artifact signs release checksums without embedding private material.
package artifact

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
)

type Signature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	SHA256    string `json:"sha256"`
	Value     string `json:"signature"`
}

func SignFile(path, keyID string, private ed25519.PrivateKey) (Signature, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Signature{}, err
	}
	if len(data) == 0 || keyID == "" || len(private) != ed25519.PrivateKeySize {
		return Signature{}, errors.New("invalid signing configuration")
	}
	digest := sha256.Sum256(data)
	sig := ed25519.Sign(private, digest[:])
	return Signature{Algorithm: "Ed25519-SHA256", KeyID: keyID, SHA256: hex.EncodeToString(digest[:]), Value: base64.StdEncoding.EncodeToString(sig)}, nil
}
func VerifyFile(path string, signature Signature, public ed25519.PublicKey) error {
	if signature.Algorithm != "Ed25519-SHA256" || len(public) != ed25519.PublicKeySize {
		return errors.New("invalid artifact signature")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	if len(data) == 0 {
		return errors.New("empty artifact")
	}
	if hex.EncodeToString(digest[:]) != signature.SHA256 {
		return errors.New("artifact digest mismatch")
	}
	raw, err := base64.StdEncoding.DecodeString(signature.Value)
	if err != nil || !ed25519.Verify(public, digest[:], raw) {
		return errors.New("artifact signature mismatch")
	}
	return nil
}
func Write(path string, signature Signature) error {
	data, err := json.MarshalIndent(signature, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
func Read(path string) (Signature, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Signature{}, err
	}
	var signature Signature
	if json.Unmarshal(data, &signature) != nil {
		return Signature{}, errors.New("invalid signature file")
	}
	return signature, nil
}
