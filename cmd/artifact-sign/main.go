package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"github.com/watchtrace/watchtrace-platform/internal/artifact"
	"os"
	"strings"
)

func main() {
	mode := flag.String("mode", "verify", "generate, sign, or verify")
	file := flag.String("file", "", "artifact path")
	key := flag.String("key", "", "base64 key file")
	signature := flag.String("signature", "", "signature JSON path")
	keyID := flag.String("key-id", "release-v1", "public key identifier")
	publicOut := flag.String("public-out", "artifact-signing.pub", "generated public key path")
	flag.Parse()
	var err error
	switch *mode {
	case "generate":
		err = generate(*key, *publicOut)
	case "sign":
		err = sign(*file, *key, *signature, *keyID)
	case "verify":
		err = verify(*file, *key, *signature)
	default:
		err = errors.New("unknown mode")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "artifact signature operation failed")
		os.Exit(1)
	}
}
func generate(privatePath, publicPath string) error {
	if privatePath == "" {
		return errors.New("private path required")
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err = os.WriteFile(privatePath, []byte(base64.StdEncoding.EncodeToString(private)), 0600); err != nil {
		return err
	}
	return os.WriteFile(publicPath, []byte(base64.StdEncoding.EncodeToString(public)), 0644)
}
func read(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
}
func sign(file, key, signature, keyID string) error {
	raw, err := read(key)
	if err != nil {
		return err
	}
	value, err := artifact.SignFile(file, keyID, ed25519.PrivateKey(raw))
	if err != nil {
		return err
	}
	return artifact.Write(signature, value)
}
func verify(file, key, signature string) error {
	raw, err := read(key)
	if err != nil {
		return err
	}
	value, err := artifact.Read(signature)
	if err != nil {
		return err
	}
	return artifact.VerifyFile(file, value, ed25519.PublicKey(raw))
}
