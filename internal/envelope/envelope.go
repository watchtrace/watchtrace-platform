// Package envelope defines the bounded, versioned cryptographic worker protocol.
package envelope

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/fxamacker/cbor/v2"
	"golang.org/x/crypto/hkdf"
)

const (
	PreviousSchemaVersion = 1
	SchemaVersion         = 2
	MaxMessageBytes       = 64 * 1024
	MaxClockSkew          = 5 * time.Second
)

var ErrInvalid = errors.New("invalid worker envelope")

var (
	canonical cbor.EncMode
	strict    cbor.DecMode
)

func init() {
	options := cbor.CanonicalEncOptions()
	options.Time = cbor.TimeRFC3339Nano
	canonical, _ = options.EncMode()
	strict, _ = cbor.DecOptions{
		DupMapKey:        cbor.DupMapKeyEnforcedAPF,
		IndefLength:      cbor.IndefLengthForbidden,
		TagsMd:           cbor.TagsForbidden,
		MaxNestedLevels:  16,
		MaxArrayElements: 128,
		MaxMapPairs:      128,
	}.DecMode()
}

type RequestLimits struct {
	MaxResponseBytes int64 `cbor:"1,keyasint" json:"max_response_bytes"`
	MaxHeaderBytes   int64 `cbor:"2,keyasint" json:"max_header_bytes"`
	MaxRedirects     int   `cbor:"3,keyasint" json:"max_redirects"`
}

type Job struct {
	SchemaVersion         int               `cbor:"1,keyasint" json:"schema_version"`
	JobID                 string            `cbor:"2,keyasint" json:"job_id"`
	JobType               string            `cbor:"3,keyasint" json:"job_type"`
	WorkerPoolID          string            `cbor:"4,keyasint" json:"worker_pool_id"`
	NetworkPolicyVersion  int               `cbor:"5,keyasint" json:"network_policy_version"`
	ScheduledAt           time.Time         `cbor:"6,keyasint" json:"scheduled_at"`
	ExpiresAt             time.Time         `cbor:"7,keyasint" json:"expires_at"`
	TargetURL             string            `cbor:"8,keyasint" json:"target_url"`
	Method                string            `cbor:"9,keyasint" json:"method"`
	TimeoutSeconds        int32             `cbor:"10,keyasint" json:"timeout_seconds"`
	ExpectedStatusMin     int16             `cbor:"11,keyasint" json:"expected_status_min"`
	ExpectedStatusMax     int16             `cbor:"12,keyasint" json:"expected_status_max"`
	Headers               map[string]string `cbor:"13,keyasint,omitempty" json:"headers,omitempty"`
	Limits                RequestLimits     `cbor:"14,keyasint" json:"limits"`
	PlatformKeyID         string            `cbor:"15,keyasint" json:"platform_key_id"`
	WorkerEncryptionKeyID string            `cbor:"16,keyasint" json:"worker_encryption_key_id"`
}

type Attributes struct {
	SchemaVersion         int       `cbor:"1,keyasint" json:"schema_version"`
	JobID                 string    `cbor:"2,keyasint" json:"job_id"`
	WorkerPoolID          string    `cbor:"3,keyasint" json:"worker_pool_id"`
	SnapshotHash          string    `cbor:"4,keyasint" json:"snapshot_hash"`
	ExpiresAt             time.Time `cbor:"5,keyasint" json:"expires_at"`
	ResultID              string    `cbor:"6,keyasint,omitempty" json:"result_id,omitempty"`
	ResultKeyID           string    `cbor:"7,keyasint,omitempty" json:"result_key_id,omitempty"`
	PlatformKeyID         string    `cbor:"8,keyasint,omitempty" json:"platform_key_id,omitempty"`
	WorkerEncryptionKeyID string    `cbor:"9,keyasint,omitempty" json:"worker_encryption_key_id,omitempty"`
}

type signedJob struct {
	Job       Job    `cbor:"1,keyasint"`
	Signature []byte `cbor:"2,keyasint"`
}

type sealedJob struct {
	SchemaVersion      int    `cbor:"1,keyasint"`
	EphemeralPublicKey []byte `cbor:"2,keyasint"`
	Nonce              []byte `cbor:"3,keyasint"`
	Ciphertext         []byte `cbor:"4,keyasint"`
}

type Result struct {
	SchemaVersion   int       `cbor:"1,keyasint" json:"schema_version"`
	ResultID        string    `cbor:"2,keyasint" json:"result_id"`
	JobID           string    `cbor:"3,keyasint" json:"job_id"`
	SnapshotHash    string    `cbor:"4,keyasint" json:"snapshot_hash"`
	WorkerPoolID    string    `cbor:"5,keyasint" json:"worker_pool_id"`
	WorkerID        string    `cbor:"6,keyasint" json:"worker_id"`
	AttemptID       string    `cbor:"7,keyasint" json:"attempt_id"`
	ScheduledAt     time.Time `cbor:"8,keyasint" json:"scheduled_at"`
	StartedAt       time.Time `cbor:"9,keyasint" json:"started_at"`
	CompletedAt     time.Time `cbor:"10,keyasint" json:"completed_at"`
	Succeeded       bool      `cbor:"11,keyasint" json:"succeeded"`
	StatusCode      *int16    `cbor:"12,keyasint,omitempty" json:"status_code"`
	ErrorCategory   *string   `cbor:"13,keyasint,omitempty" json:"error_category"`
	DNSMicros       *int64    `cbor:"14,keyasint,omitempty" json:"dns_us"`
	ConnectMicros   *int64    `cbor:"15,keyasint,omitempty" json:"connect_us"`
	TLSMicros       *int64    `cbor:"16,keyasint,omitempty" json:"tls_us"`
	FirstByteMicros *int64    `cbor:"17,keyasint,omitempty" json:"first_byte_us"`
	TotalMicros     int64     `cbor:"18,keyasint" json:"total_us"`
	ResultKeyID     string    `cbor:"19,keyasint" json:"result_key_id"`
}

type SignedResult struct {
	Result    Result `cbor:"1,keyasint" json:"result"`
	Signature []byte `cbor:"2,keyasint" json:"signature"`
}

type ExpiredAcknowledgement struct {
	SchemaVersion int       `cbor:"1,keyasint" json:"schema_version"`
	JobID         string    `cbor:"2,keyasint" json:"job_id"`
	SnapshotHash  string    `cbor:"3,keyasint" json:"snapshot_hash"`
	WorkerPoolID  string    `cbor:"4,keyasint" json:"worker_pool_id"`
	WorkerID      string    `cbor:"5,keyasint" json:"worker_id"`
	ExpiredAt     time.Time `cbor:"6,keyasint" json:"expired_at"`
	ResultKeyID   string    `cbor:"7,keyasint" json:"result_key_id"`
}

type signedExpired struct {
	Acknowledgement ExpiredAcknowledgement `cbor:"1,keyasint"`
	Signature       []byte                 `cbor:"2,keyasint"`
}

func SupportsSchema(version int) bool {
	return version == PreviousSchemaVersion || version == SchemaVersion
}

func SealJob(job Job, signer ed25519.PrivateKey, workerPublic *ecdh.PublicKey) ([]byte, Attributes, error) {
	if len(signer) != ed25519.PrivateKeySize || workerPublic == nil || validateJob(job, time.Now().UTC(), true) != nil {
		return nil, Attributes{}, ErrInvalid
	}
	plain, err := canonical.Marshal(job)
	if err != nil {
		return nil, Attributes{}, ErrInvalid
	}
	hash := sha256.Sum256(plain)
	attrs := Attributes{SchemaVersion: job.SchemaVersion, JobID: job.JobID, WorkerPoolID: job.WorkerPoolID, SnapshotHash: hex.EncodeToString(hash[:]), ExpiresAt: job.ExpiresAt, PlatformKeyID: job.PlatformKeyID, WorkerEncryptionKeyID: job.WorkerEncryptionKeyID}
	aad, err := canonical.Marshal(attrs)
	if err != nil {
		return nil, Attributes{}, ErrInvalid
	}
	signedBytes, err := canonical.Marshal(signedJob{Job: job, Signature: ed25519.Sign(signer, appendDomain("watchtrace-job-signature", aad, plain))})
	if err != nil {
		return nil, Attributes{}, ErrInvalid
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, Attributes{}, err
	}
	secret, err := eph.ECDH(workerPublic)
	if err != nil {
		return nil, Attributes{}, ErrInvalid
	}
	key, err := deriveKey(secret, eph.PublicKey().Bytes(), workerPublic.Bytes(), job.SchemaVersion)
	if err != nil {
		return nil, Attributes{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, Attributes{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, Attributes{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, Attributes{}, err
	}
	body, err := canonical.Marshal(sealedJob{SchemaVersion: job.SchemaVersion, EphemeralPublicKey: eph.PublicKey().Bytes(), Nonce: nonce, Ciphertext: aead.Seal(nil, nonce, signedBytes, aad)})
	if err != nil || len(body) == 0 || len(body) > MaxMessageBytes {
		return nil, Attributes{}, ErrInvalid
	}
	return body, attrs, nil
}

func OpenJob(data []byte, attrs Attributes, workerPrivate *ecdh.PrivateKey, platformPublic ed25519.PublicKey, now time.Time, tolerance time.Duration) (Job, error) {
	if len(data) == 0 || len(data) > MaxMessageBytes || !SupportsSchema(attrs.SchemaVersion) || workerPrivate == nil || len(platformPublic) != ed25519.PublicKeySize || tolerance < 0 || tolerance > 30*time.Second {
		return Job{}, ErrInvalid
	}
	var sealed sealedJob
	if strict.Unmarshal(data, &sealed) != nil || sealed.SchemaVersion != attrs.SchemaVersion || len(sealed.EphemeralPublicKey) != 32 || len(sealed.Nonce) != 12 {
		return Job{}, ErrInvalid
	}
	eph, err := ecdh.X25519().NewPublicKey(sealed.EphemeralPublicKey)
	if err != nil {
		return Job{}, ErrInvalid
	}
	secret, err := workerPrivate.ECDH(eph)
	if err != nil {
		return Job{}, ErrInvalid
	}
	key, err := deriveKey(secret, sealed.EphemeralPublicKey, workerPrivate.PublicKey().Bytes(), sealed.SchemaVersion)
	if err != nil {
		return Job{}, ErrInvalid
	}
	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	aad, err := canonical.Marshal(attrs)
	if err != nil {
		return Job{}, ErrInvalid
	}
	plain, err := aead.Open(nil, sealed.Nonce, sealed.Ciphertext, aad)
	if err != nil {
		return Job{}, ErrInvalid
	}
	var signed signedJob
	if strict.Unmarshal(plain, &signed) != nil || len(signed.Signature) != ed25519.SignatureSize {
		return Job{}, ErrInvalid
	}
	jobBytes, err := canonical.Marshal(signed.Job)
	if err != nil || !ed25519.Verify(platformPublic, appendDomain("watchtrace-job-signature", aad, jobBytes), signed.Signature) {
		return Job{}, ErrInvalid
	}
	hash := sha256.Sum256(jobBytes)
	if hex.EncodeToString(hash[:]) != attrs.SnapshotHash || signed.Job.SchemaVersion != attrs.SchemaVersion || signed.Job.JobID != attrs.JobID || signed.Job.WorkerPoolID != attrs.WorkerPoolID || signed.Job.PlatformKeyID != attrs.PlatformKeyID || signed.Job.WorkerEncryptionKeyID != attrs.WorkerEncryptionKeyID || !signed.Job.ExpiresAt.Equal(attrs.ExpiresAt) || validateJob(signed.Job, now.Add(-tolerance), false) != nil {
		return Job{}, ErrInvalid
	}
	return signed.Job, nil
}

func SignResult(result Result, key ed25519.PrivateKey) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize || validateResult(result) != nil {
		return nil, ErrInvalid
	}
	raw, err := canonical.Marshal(result)
	if err != nil {
		return nil, ErrInvalid
	}
	out, err := canonical.Marshal(SignedResult{Result: result, Signature: ed25519.Sign(key, appendDomain("watchtrace-result-signature", raw))})
	if err != nil || len(out) > MaxMessageBytes {
		return nil, ErrInvalid
	}
	return out, nil
}

func PeekResult(data []byte) (Result, error) {
	if len(data) == 0 || len(data) > MaxMessageBytes {
		return Result{}, ErrInvalid
	}
	var signed SignedResult
	if strict.Unmarshal(data, &signed) != nil || validateResult(signed.Result) != nil {
		return Result{}, ErrInvalid
	}
	return signed.Result, nil
}

func VerifyResult(data []byte, key ed25519.PublicKey) (Result, error) {
	result, err := PeekResult(data)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return Result{}, ErrInvalid
	}
	var signed SignedResult
	if strict.Unmarshal(data, &signed) != nil || len(signed.Signature) != ed25519.SignatureSize {
		return Result{}, ErrInvalid
	}
	raw, _ := canonical.Marshal(signed.Result)
	if !ed25519.Verify(key, appendDomain("watchtrace-result-signature", raw), signed.Signature) {
		return Result{}, ErrInvalid
	}
	return result, nil
}

func ResultAttributes(result Result) Attributes {
	return Attributes{SchemaVersion: result.SchemaVersion, JobID: result.JobID, WorkerPoolID: result.WorkerPoolID, SnapshotHash: result.SnapshotHash, ResultID: result.ResultID, ResultKeyID: result.ResultKeyID}
}

func SignExpired(value ExpiredAcknowledgement, key ed25519.PrivateKey) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize || validateExpired(value) != nil {
		return nil, ErrInvalid
	}
	raw, _ := canonical.Marshal(value)
	out, err := canonical.Marshal(signedExpired{Acknowledgement: value, Signature: ed25519.Sign(key, appendDomain("watchtrace-expired-signature", raw))})
	if err != nil || len(out) > MaxMessageBytes {
		return nil, ErrInvalid
	}
	return out, nil
}

func VerifyExpired(data []byte, key ed25519.PublicKey) (ExpiredAcknowledgement, error) {
	var signed signedExpired
	if len(data) == 0 || len(data) > MaxMessageBytes || len(key) != ed25519.PublicKeySize || strict.Unmarshal(data, &signed) != nil || validateExpired(signed.Acknowledgement) != nil {
		return ExpiredAcknowledgement{}, ErrInvalid
	}
	raw, _ := canonical.Marshal(signed.Acknowledgement)
	if !ed25519.Verify(key, appendDomain("watchtrace-expired-signature", raw), signed.Signature) {
		return ExpiredAcknowledgement{}, ErrInvalid
	}
	return signed.Acknowledgement, nil
}

func deriveKey(secret, ephemeralPublic, workerPublic []byte, schema int) ([]byte, error) {
	info := appendDomain(fmt.Sprintf("watchtrace-job-hkdf-v%d", schema), ephemeralPublic, workerPublic)
	reader := hkdf.New(sha256.New, secret, nil, info)
	key := make([]byte, 32)
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, err
	}
	return key, nil
}

func appendDomain(domain string, parts ...[]byte) []byte {
	total := len(domain) + 1
	for _, part := range parts {
		total += 4 + len(part)
	}
	out := make([]byte, 0, total)
	out = append(out, domain...)
	out = append(out, 0)
	for _, part := range parts {
		length := uint32(len(part))
		out = append(out, byte(length>>24), byte(length>>16), byte(length>>8), byte(length))
		out = append(out, part...)
	}
	return out
}

func validateJob(job Job, now time.Time, requireFuture bool) error {
	if !SupportsSchema(job.SchemaVersion) || job.JobID == "" || job.WorkerPoolID == "" || job.NetworkPolicyVersion < 1 || job.PlatformKeyID == "" || job.WorkerEncryptionKeyID == "" || (job.JobType != "scheduled" && job.JobType != "manual_test") || (job.Method != "GET" && job.Method != "HEAD") || job.TimeoutSeconds < 1 || job.TimeoutSeconds > 10 || job.ExpectedStatusMin < 100 || job.ExpectedStatusMax > 599 || job.ExpectedStatusMin > job.ExpectedStatusMax || len(job.TargetURL) == 0 || len(job.TargetURL) > 2048 || job.ScheduledAt.IsZero() || job.ExpiresAt.Before(job.ScheduledAt) || job.ExpiresAt.Sub(job.ScheduledAt) > 2*time.Minute || job.Limits.MaxResponseBytes < 1 || job.Limits.MaxResponseBytes > 1024*1024 || job.Limits.MaxHeaderBytes < 1 || job.Limits.MaxHeaderBytes > 64*1024 || job.Limits.MaxRedirects < 0 || job.Limits.MaxRedirects > 5 || len(job.Headers) > 32 {
		return ErrInvalid
	}
	if requireFuture && job.ExpiresAt.Before(now) {
		return fmt.Errorf("%w: expired", ErrInvalid)
	}
	return nil
}

func validateResult(result Result) error {
	if !SupportsSchema(result.SchemaVersion) || result.ResultID == "" || result.JobID == "" || len(result.SnapshotHash) != 64 || result.WorkerPoolID == "" || result.WorkerID == "" || result.AttemptID == "" || result.ResultKeyID == "" || result.ScheduledAt.IsZero() || result.StartedAt.IsZero() || result.CompletedAt.Before(result.StartedAt) || result.TotalMicros < 0 || (result.StatusCode != nil && (*result.StatusCode < 100 || *result.StatusCode > 599)) || (!result.Succeeded && result.ErrorCategory == nil) || (result.Succeeded && (result.StatusCode == nil || result.ErrorCategory != nil)) {
		return ErrInvalid
	}
	return nil
}

func validateExpired(value ExpiredAcknowledgement) error {
	if !SupportsSchema(value.SchemaVersion) || value.JobID == "" || len(value.SnapshotHash) != 64 || value.WorkerPoolID == "" || value.WorkerID == "" || value.ResultKeyID == "" || value.ExpiredAt.IsZero() {
		return ErrInvalid
	}
	return nil
}
