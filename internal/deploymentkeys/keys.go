// Package deploymentkeys generates and verifies the purpose-separated keys
// required by the private WatchTrace deployment.
package deploymentkeys

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	PlatformSigningPrivate = "platform-signing-private"
	PlatformSigningPublic  = "platform-signing-public"
	MonitorHeaderKey       = "monitor-header-key"
	QuarantineKey          = "quarantine-key"
	WorkerEncryption       = "worker-encryption-private"
	WorkerResult           = "worker-result-private"
	HostedPublicBundle     = "hosted-public.json"
)

var generatedFiles = []string{
	PlatformSigningPrivate,
	PlatformSigningPublic,
	MonitorHeaderKey,
	QuarantineKey,
	WorkerEncryption,
	WorkerResult,
	HostedPublicBundle,
}

type publicBundle struct {
	PoolID              string `json:"pool_id"`
	EncryptionKeyID     string `json:"encryption_key_id"`
	ResultKeyID         string `json:"result_key_id"`
	EncryptionPublicKey string `json:"encryption_public_key"`
	ResultPublicKey     string `json:"result_public_key"`
}

// Generate creates one complete key set in an existing empty directory. It
// never overwrites a file and never writes key material to standard output.
func Generate(directory string) error {
	if err := requireEmptyDirectory(directory); err != nil {
		return err
	}

	platformPublic, platformPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return errors.New("generate platform signing key")
	}
	workerEncryption, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return errors.New("generate worker encryption key")
	}
	workerResultPublic, workerResultPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return errors.New("generate worker result key")
	}
	monitorHeader := make([]byte, 32)
	if _, err = rand.Read(monitorHeader); err != nil {
		return errors.New("generate monitor header key")
	}
	quarantine := make([]byte, 32)
	if _, err = rand.Read(quarantine); err != nil {
		return errors.New("generate quarantine key")
	}

	values := map[string][]byte{
		PlatformSigningPrivate: encoded(platformPrivate),
		PlatformSigningPublic:  encoded(platformPublic),
		MonitorHeaderKey:       encoded(monitorHeader),
		QuarantineKey:          encoded(quarantine),
		WorkerEncryption:       encoded(workerEncryption.Bytes()),
		WorkerResult:           encoded(workerResultPrivate),
	}
	for _, name := range generatedFiles[:6] {
		if err = writeExclusive(filepath.Join(directory, name), values[name]); err != nil {
			return err
		}
	}

	bundle, err := json.MarshalIndent(publicBundle{
		PoolID:              "hosted",
		EncryptionKeyID:     "enc-v1",
		ResultKeyID:         "result-v1",
		EncryptionPublicKey: base64.StdEncoding.EncodeToString(workerEncryption.PublicKey().Bytes()),
		ResultPublicKey:     base64.StdEncoding.EncodeToString(workerResultPublic),
	}, "", "  ")
	if err != nil {
		return errors.New("encode hosted public bundle")
	}
	if err = writeExclusive(filepath.Join(directory, HostedPublicBundle), append(bundle, '\n')); err != nil {
		return err
	}
	return Verify(directory)
}

// Verify checks key encodings, sizes, private/public relationships, and the
// hosted public bundle without exposing any key value.
func Verify(directory string) error {
	platformPrivate, err := readBase64(directory, PlatformSigningPrivate, ed25519.PrivateKeySize)
	if err != nil {
		return err
	}
	platformPublic, err := readBase64(directory, PlatformSigningPublic, ed25519.PublicKeySize)
	if err != nil {
		return err
	}
	if !bytes.Equal(ed25519.PrivateKey(platformPrivate).Public().(ed25519.PublicKey), platformPublic) {
		return errors.New("platform signing key pair does not match")
	}
	if _, err = readBase64(directory, MonitorHeaderKey, 32); err != nil {
		return err
	}
	if _, err = readBase64(directory, QuarantineKey, 32); err != nil {
		return err
	}
	workerEncryptionRaw, err := readBase64(directory, WorkerEncryption, 32)
	if err != nil {
		return err
	}
	workerEncryption, err := ecdh.X25519().NewPrivateKey(workerEncryptionRaw)
	if err != nil {
		return errors.New("worker encryption key is invalid")
	}
	workerResult, err := readBase64(directory, WorkerResult, ed25519.PrivateKeySize)
	if err != nil {
		return err
	}

	bundleData, err := os.ReadFile(filepath.Join(directory, HostedPublicBundle))
	if err != nil {
		return errors.New("read hosted public bundle")
	}
	var bundle publicBundle
	if json.Unmarshal(bundleData, &bundle) != nil || bundle.PoolID != "hosted" ||
		bundle.EncryptionKeyID != "enc-v1" || bundle.ResultKeyID != "result-v1" {
		return errors.New("hosted public bundle is invalid")
	}
	if bundle.EncryptionPublicKey != base64.StdEncoding.EncodeToString(workerEncryption.PublicKey().Bytes()) ||
		bundle.ResultPublicKey != base64.StdEncoding.EncodeToString(ed25519.PrivateKey(workerResult).Public().(ed25519.PublicKey)) {
		return errors.New("hosted public bundle does not match private keys")
	}
	return nil
}

func requireEmptyDirectory(directory string) error {
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return errors.New("output directory must already exist")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return errors.New("read output directory")
	}
	if len(entries) != 0 {
		return errors.New("output directory must be empty; existing keys are never overwritten")
	}
	return nil
}

func encoded(value []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(value))
}

func writeExclusive(path string, value []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create %s", filepath.Base(path))
	}
	if _, err = file.Write(value); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s", filepath.Base(path))
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync %s", filepath.Base(path))
	}
	if err = file.Close(); err != nil {
		return fmt.Errorf("close %s", filepath.Base(path))
	}
	return nil
}

func readBase64(directory, name string, size int) ([]byte, error) {
	value, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		return nil, fmt.Errorf("read %s", name)
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(string(value)))
	if err != nil || len(decoded) != size {
		return nil, fmt.Errorf("%s has an invalid encoding or size", name)
	}
	return decoded, nil
}
