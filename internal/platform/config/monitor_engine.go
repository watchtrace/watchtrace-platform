package config

import (
	"crypto/ed25519"
	"fmt"
)

const (
	monitorSigningKeyEnvironment    = "WATCHTRACE_PLATFORM_SIGNING_KEY"
	monitorSigningKeyIDEnvironment  = "WATCHTRACE_PLATFORM_SIGNING_KEY_ID"
	engineHeaderKeyFileEnvironment  = "WATCHTRACE_MONITOR_HEADER_KEY_FILE"
	monitorQuarantineKeyEnvironment = "WATCHTRACE_QUARANTINE_KEY"
	monitorJobQueueEnvironment      = "WATCHTRACE_SQS_HOSTED_JOB_QUEUE_URL"
	monitorResultQueueEnvironment   = "WATCHTRACE_SQS_RESULT_QUEUE_URL"
	monitorJobDLQEnvironment        = "WATCHTRACE_SQS_HOSTED_JOB_DLQ_URL"
	monitorResultDLQEnvironment     = "WATCHTRACE_SQS_RESULT_DLQ_URL"
	monitorSQSEndpointEnvironment   = "WATCHTRACE_SQS_ENDPOINT"
	monitorHealthAddressEnvironment = "WATCHTRACE_ENGINE_HEALTH_ADDRESS"
	defaultMonitorHealthAddress     = "127.0.0.1:8091"
)

// MonitorEngineConfig contains every setting the monitoring engine needs.
type MonitorEngineConfig struct {
	DatabaseURL    string
	SigningKey     ed25519.PrivateKey
	SigningKeyID   string
	HeaderKey      []byte
	QuarantineKey  []byte
	JobQueueURL    string
	ResultQueueURL string
	JobDLQURL      string
	ResultDLQURL   string
	SQSEndpoint    string
	HealthAddress  string
}

func LoadMonitorEngine() (MonitorEngineConfig, error) {
	return loadMonitorEngine(systemCommandSource())
}

func loadMonitorEngine(source commandSource) (MonitorEngineConfig, error) {
	databaseURL, err := loadDatabaseURL(source.lookup)
	if err != nil {
		return MonitorEngineConfig{}, err
	}
	signingKey, err := source.base64KeyFile(monitorSigningKeyEnvironment, ed25519.PrivateKeySize)
	if err != nil {
		return MonitorEngineConfig{}, err
	}
	headerKey, err := source.base64KeyFile(engineHeaderKeyFileEnvironment, 32)
	if err != nil {
		return MonitorEngineConfig{}, err
	}
	quarantineKey, err := source.base64KeyFile(monitorQuarantineKeyEnvironment, 32)
	if err != nil {
		return MonitorEngineConfig{}, err
	}
	signingKeyID := source.value(monitorSigningKeyIDEnvironment, "platform-v1")
	if len(signingKeyID) > 64 {
		return MonitorEngineConfig{}, fmt.Errorf("%s must not exceed 64 bytes", monitorSigningKeyIDEnvironment)
	}
	jobQueueURL, err := source.required(monitorJobQueueEnvironment)
	if err != nil {
		return MonitorEngineConfig{}, err
	}
	resultQueueURL, err := source.required(monitorResultQueueEnvironment)
	if err != nil {
		return MonitorEngineConfig{}, err
	}
	jobDLQURL, err := source.required(monitorJobDLQEnvironment)
	if err != nil {
		return MonitorEngineConfig{}, err
	}
	resultDLQURL, err := source.required(monitorResultDLQEnvironment)
	if err != nil {
		return MonitorEngineConfig{}, err
	}
	healthAddress := source.value(monitorHealthAddressEnvironment, defaultMonitorHealthAddress)
	if err = validateListenAddress(monitorHealthAddressEnvironment, healthAddress); err != nil {
		return MonitorEngineConfig{}, err
	}
	return MonitorEngineConfig{
		DatabaseURL: databaseURL, SigningKey: ed25519.PrivateKey(signingKey), SigningKeyID: signingKeyID,
		HeaderKey: headerKey, QuarantineKey: quarantineKey,
		JobQueueURL: jobQueueURL, ResultQueueURL: resultQueueURL, JobDLQURL: jobDLQURL, ResultDLQURL: resultDLQURL,
		SQSEndpoint: source.optional(monitorSQSEndpointEnvironment), HealthAddress: healthAddress,
	}, nil
}
