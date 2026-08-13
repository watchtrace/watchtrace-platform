// Package gatewayconfig verifies the signed, database-free gateway snapshot.
package gatewayconfig

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

type Limits struct {
	RequestsPerMinute int64 `json:"requests_per_minute"`
	BytesPerMinute    int64 `json:"bytes_per_minute"`
	ConcurrentPulls   int64 `json:"concurrent_pulls"`
	ResultsPerMinute  int64 `json:"results_per_minute"`
}

type Pool struct {
	ID                        string   `json:"id"`
	JobQueueURL               string   `json:"job_queue_url"`
	ResultKeyID               string   `json:"result_key_id"`
	ResultPublicKey           string   `json:"result_public_key"`
	SchemaMin                 int      `json:"schema_min"`
	SchemaMax                 int      `json:"schema_max"`
	Capabilities              []string `json:"capabilities"`
	RevokedCertificateSerials []string `json:"revoked_certificate_serials"`
	Limits                    Limits   `json:"limits"`
}

type Snapshot struct {
	Version        int       `json:"version"`
	Environment    string    `json:"environment"`
	Region         string    `json:"region"`
	ResultQueueURL string    `json:"result_queue_url"`
	GeneratedAt    time.Time `json:"generated_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	Pools          []Pool    `json:"pools"`
}

type Signed struct {
	Snapshot  Snapshot `json:"snapshot"`
	Signature string   `json:"signature"`
}

func Sign(snapshot Snapshot, key ed25519.PrivateKey) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize || validate(snapshot, time.Now().UTC()) != nil {
		return nil, errors.New("invalid gateway configuration")
	}
	raw, _ := json.Marshal(snapshot)
	return json.MarshalIndent(Signed{Snapshot: snapshot, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, append([]byte("watchtrace-gateway-config-v1\x00"), raw...)))}, "", "  ")
}

func Verify(data []byte, key ed25519.PublicKey, now time.Time) (Snapshot, error) {
	if len(data) == 0 || len(data) > 1024*1024 || len(key) != ed25519.PublicKeySize {
		return Snapshot{}, errors.New("invalid gateway configuration")
	}
	var signed Signed
	if json.Unmarshal(data, &signed) != nil || validate(signed.Snapshot, now.UTC()) != nil {
		return Snapshot{}, errors.New("invalid gateway configuration")
	}
	raw, _ := json.Marshal(signed.Snapshot)
	signature, err := base64.StdEncoding.DecodeString(signed.Signature)
	if err != nil || !ed25519.Verify(key, append([]byte("watchtrace-gateway-config-v1\x00"), raw...), signature) {
		return Snapshot{}, errors.New("invalid gateway configuration")
	}
	return signed.Snapshot, nil
}

func validate(snapshot Snapshot, now time.Time) error {
	if snapshot.Version != 1 || snapshot.Environment == "" || snapshot.Region == "" || snapshot.ResultQueueURL == "" || snapshot.GeneratedAt.IsZero() || snapshot.ExpiresAt.IsZero() || snapshot.ExpiresAt.Before(now) || snapshot.ExpiresAt.Sub(snapshot.GeneratedAt) > 35*24*time.Hour || len(snapshot.Pools) == 0 || len(snapshot.Pools) > 6 {
		return errors.New("invalid gateway configuration")
	}
	seen := map[string]struct{}{}
	for _, pool := range snapshot.Pools {
		public, err := base64.StdEncoding.DecodeString(pool.ResultPublicKey)
		if pool.ID == "" || pool.JobQueueURL == "" || pool.ResultKeyID == "" || err != nil || len(public) != ed25519.PublicKeySize || pool.SchemaMin < 1 || pool.SchemaMax > 2 || pool.SchemaMin > pool.SchemaMax || pool.Limits.RequestsPerMinute < 1 || pool.Limits.BytesPerMinute < 1024 || pool.Limits.ConcurrentPulls < 1 || pool.Limits.ResultsPerMinute < 1 {
			return errors.New("invalid gateway pool")
		}
		if _, exists := seen[pool.ID]; exists {
			return errors.New("duplicate gateway pool")
		}
		seen[pool.ID] = struct{}{}
	}
	return nil
}
