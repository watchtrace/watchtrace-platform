package config

import (
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

const (
	WorkerTransportHTTPS     = "https"
	WorkerTransportDirectSQS = "direct_sqs"

	workerPoolIDEnvironment          = "WATCHTRACE_WORKER_POOL_ID"
	workerIDEnvironment              = "WATCHTRACE_WORKER_ID"
	workerEncryptionKeyEnvironment   = "WATCHTRACE_WORKER_ENCRYPTION_KEY"
	workerEncryptionKeyIDEnvironment = "WATCHTRACE_WORKER_ENCRYPTION_KEY_ID"
	workerResultKeyEnvironment       = "WATCHTRACE_WORKER_RESULT_KEY"
	workerResultKeyIDEnvironment     = "WATCHTRACE_RESULT_KEY_ID"
	workerPlatformKeyEnvironment     = "WATCHTRACE_PLATFORM_SIGNING_PUBLIC_KEY"
	workerPlatformKeyIDEnvironment   = "WATCHTRACE_PLATFORM_SIGNING_KEY_ID"
	workerKeyringEnvironment         = "WATCHTRACE_WORKER_KEYRING"
	workerJournalEnvironment         = "WATCHTRACE_WORKER_JOURNAL"
	workerPrivateCIDRsEnvironment    = "WATCHTRACE_PRIVATE_CIDRS"
	workerTransportEnvironment       = "WATCHTRACE_WORKER_TRANSPORT"
	workerJobQueueEnvironment        = "WATCHTRACE_SQS_HOSTED_JOB_QUEUE_URL"
	workerResultQueueEnvironment     = "WATCHTRACE_SQS_RESULT_QUEUE_URL"
	workerSQSEndpointEnvironment     = "WATCHTRACE_SQS_ENDPOINT"
	workerGatewayURLEnvironment      = "WATCHTRACE_GATEWAY_URL"
	workerPoolTokenEnvironment       = "WATCHTRACE_POOL_TOKEN"
	workerMTLSCertificateEnvironment = "WATCHTRACE_MTLS_CERT"
	workerMTLSKeyEnvironment         = "WATCHTRACE_MTLS_KEY"
	workerGatewayCAEnvironment       = "WATCHTRACE_GATEWAY_CA"
	workerHealthAddressEnvironment   = "WATCHTRACE_WORKER_HEALTH_ADDRESS"
	workerClockOffsetEnvironment     = "WATCHTRACE_CLOCK_OFFSET"
)

type WorkerKeyring struct {
	WorkerEncryption map[string][]byte
	PlatformSigning  map[string]ed25519.PublicKey
	Revoked          map[string]struct{}
}

type WorkerConfig struct {
	PoolID, WorkerID, EncryptionKeyID, ResultKeyID, PlatformKeyID string
	EncryptionKey, ResultKey                                      []byte
	PlatformPublicKey                                             ed25519.PublicKey
	Keyring                                                       WorkerKeyring
	JournalPath                                                   string
	PrivateCIDRs                                                  []netip.Prefix
	Transport                                                     string
	JobQueueURL, ResultQueueURL, SQSEndpoint                      string
	GatewayURL, PoolToken                                         string
	ClientTLS                                                     *tls.Config
	HealthAddress                                                 string
	ClockOffset                                                   time.Duration
}

func LoadWorker() (WorkerConfig, error) { return loadWorker(systemCommandSource()) }

func loadWorker(source commandSource) (WorkerConfig, error) {
	configuration := WorkerConfig{Keyring: WorkerKeyring{
		WorkerEncryption: map[string][]byte{}, PlatformSigning: map[string]ed25519.PublicKey{}, Revoked: map[string]struct{}{},
	}}
	var err error
	if configuration.PoolID, err = source.required(workerPoolIDEnvironment); err != nil {
		return WorkerConfig{}, err
	}
	if configuration.WorkerID, err = source.required(workerIDEnvironment); err != nil {
		return WorkerConfig{}, err
	}
	if configuration.EncryptionKeyID, err = source.required(workerEncryptionKeyIDEnvironment); err != nil {
		return WorkerConfig{}, err
	}
	if configuration.ResultKeyID, err = source.required(workerResultKeyIDEnvironment); err != nil {
		return WorkerConfig{}, err
	}
	if configuration.PlatformKeyID, err = source.required(workerPlatformKeyIDEnvironment); err != nil {
		return WorkerConfig{}, err
	}
	if configuration.EncryptionKey, err = source.base64KeyFile(workerEncryptionKeyEnvironment, 32); err != nil {
		return WorkerConfig{}, err
	}
	if configuration.ResultKey, err = source.base64KeyFile(workerResultKeyEnvironment, ed25519.PrivateKeySize); err != nil {
		return WorkerConfig{}, err
	}
	platformKey, err := source.base64KeyFile(workerPlatformKeyEnvironment, ed25519.PublicKeySize)
	if err != nil {
		return WorkerConfig{}, err
	}
	configuration.PlatformPublicKey = ed25519.PublicKey(platformKey)
	configuration.JournalPath = source.value(workerJournalEnvironment, "/var/lib/watchtrace-worker/journal.sqlite")
	configuration.HealthAddress = source.value(workerHealthAddressEnvironment, "127.0.0.1:8090")
	if err = validateListenAddress(workerHealthAddressEnvironment, configuration.HealthAddress); err != nil {
		return WorkerConfig{}, err
	}
	configuration.ClockOffset, err = parseDuration(workerClockOffsetEnvironment, source.value(workerClockOffsetEnvironment, "0s"), 24*time.Hour)
	if err != nil {
		return WorkerConfig{}, err
	}
	for _, raw := range strings.Split(source.optional(workerPrivateCIDRsEnvironment), ",") {
		if raw = strings.TrimSpace(raw); raw != "" {
			prefix, parseErr := netip.ParsePrefix(raw)
			if parseErr != nil {
				return WorkerConfig{}, fmt.Errorf("%s contains an invalid prefix", workerPrivateCIDRsEnvironment)
			}
			configuration.PrivateCIDRs = append(configuration.PrivateCIDRs, prefix)
		}
	}
	if err = loadWorkerKeyring(source, &configuration); err != nil {
		return WorkerConfig{}, err
	}
	configuration.Transport = source.value(workerTransportEnvironment, WorkerTransportHTTPS)
	switch configuration.Transport {
	case WorkerTransportDirectSQS:
		if configuration.JobQueueURL, err = source.required(workerJobQueueEnvironment); err != nil {
			return WorkerConfig{}, err
		}
		if configuration.ResultQueueURL, err = source.required(workerResultQueueEnvironment); err != nil {
			return WorkerConfig{}, err
		}
		configuration.SQSEndpoint = source.optional(workerSQSEndpointEnvironment)
	case WorkerTransportHTTPS:
		if configuration.GatewayURL, err = source.required(workerGatewayURLEnvironment); err != nil {
			return WorkerConfig{}, err
		}
		if err = validateHTTPSURL(workerGatewayURLEnvironment, configuration.GatewayURL); err != nil {
			return WorkerConfig{}, err
		}
		configuration.PoolToken = source.optional(workerPoolTokenEnvironment)
		configuration.ClientTLS, err = source.clientTLS(workerMTLSCertificateEnvironment, workerMTLSKeyEnvironment, workerGatewayCAEnvironment)
		if err != nil {
			return WorkerConfig{}, err
		}
	default:
		return WorkerConfig{}, errors.New("WATCHTRACE_WORKER_TRANSPORT must be https or direct_sqs")
	}
	return configuration, nil
}

func loadWorkerKeyring(source commandSource, configuration *WorkerConfig) error {
	path := source.optional(workerKeyringEnvironment)
	if path == "" {
		return nil
	}
	data, err := source.readFile(path)
	if err != nil {
		return errors.New("WATCHTRACE_WORKER_KEYRING could not be read")
	}
	var keyring struct {
		WorkerEncryption map[string]string `json:"worker_encryption"`
		PlatformSigning  map[string]string `json:"platform_signing"`
		Revoked          []string          `json:"revoked"`
	}
	if err = json.Unmarshal(data, &keyring); err != nil {
		return errors.New("WATCHTRACE_WORKER_KEYRING is invalid")
	}
	for id, keyPath := range keyring.WorkerEncryption {
		if id == "" {
			return errors.New("WATCHTRACE_WORKER_KEYRING is invalid")
		}
		key, keyErr := source.base64KeyPath(keyPath, workerKeyringEnvironment, 32)
		if keyErr != nil {
			return keyErr
		}
		configuration.Keyring.WorkerEncryption[id] = key
	}
	for id, keyPath := range keyring.PlatformSigning {
		if id == "" {
			return errors.New("WATCHTRACE_WORKER_KEYRING is invalid")
		}
		key, keyErr := source.base64KeyPath(keyPath, workerKeyringEnvironment, ed25519.PublicKeySize)
		if keyErr != nil {
			return keyErr
		}
		configuration.Keyring.PlatformSigning[id] = ed25519.PublicKey(key)
	}
	for _, id := range keyring.Revoked {
		if strings.TrimSpace(id) == "" {
			return errors.New("WATCHTRACE_WORKER_KEYRING is invalid")
		}
		configuration.Keyring.Revoked[id] = struct{}{}
	}
	return nil
}
