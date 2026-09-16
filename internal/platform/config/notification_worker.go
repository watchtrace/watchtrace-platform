package config

import (
	"errors"
	"strings"
	"time"

	platformmail "github.com/watchtrace/watchtrace-platform/internal/platform/mail"
)

const (
	notificationProviderEnvironment      = "WATCHTRACE_NOTIFICATION_PROVIDER"
	notificationSMTPAddressEnvironment   = "WATCHTRACE_NOTIFICATION_SMTP_ADDRESS"
	notificationSMTPUsernameEnvironment  = "WATCHTRACE_NOTIFICATION_SMTP_USERNAME"
	notificationSMTPPasswordEnvironment  = "WATCHTRACE_NOTIFICATION_SMTP_PASSWORD"
	notificationFromEnvironment          = "WATCHTRACE_NOTIFICATION_FROM"
	notificationWorkerIDEnvironment      = "WATCHTRACE_NOTIFICATION_WORKER_ID"
	notificationHealthAddressEnvironment = "WATCHTRACE_NOTIFICATION_HEALTH_ADDRESS"
	defaultNotificationHealthAddress     = "127.0.0.1:8092"
)

type NotificationWorkerConfig struct {
	DatabaseURL, Provider, SMTPAddress, SMTPUsername, SMTPPassword string
	From, WorkerID, HealthAddress                                  string
}

func LoadNotificationWorker() (NotificationWorkerConfig, error) {
	return loadNotificationWorker(systemCommandSource())
}

func loadNotificationWorker(source commandSource) (NotificationWorkerConfig, error) {
	databaseURL, err := loadDatabaseURL(source.lookup)
	if err != nil {
		return NotificationWorkerConfig{}, err
	}
	configuration := NotificationWorkerConfig{
		DatabaseURL:   databaseURL,
		Provider:      strings.ToLower(source.value(notificationProviderEnvironment, "local")),
		SMTPAddress:   source.value(notificationSMTPAddressEnvironment, "127.0.0.1:1025"),
		SMTPUsername:  source.optional(notificationSMTPUsernameEnvironment),
		SMTPPassword:  source.optional(notificationSMTPPasswordEnvironment),
		From:          source.value(notificationFromEnvironment, "watchtrace@localhost"),
		WorkerID:      source.value(notificationWorkerIDEnvironment, "notification-worker-1"),
		HealthAddress: source.value(notificationHealthAddressEnvironment, defaultNotificationHealthAddress),
	}
	if err = validateListenAddress(notificationHealthAddressEnvironment, configuration.HealthAddress); err != nil {
		return NotificationWorkerConfig{}, err
	}
	switch configuration.Provider {
	case "local":
		_, err = platformmail.NewLocal(configuration.SMTPAddress, configuration.From, time.Second)
	case "oci":
		_, err = platformmail.NewOCI(configuration.SMTPAddress, configuration.SMTPUsername, configuration.SMTPPassword, configuration.From, time.Second)
	default:
		return NotificationWorkerConfig{}, errors.New("WATCHTRACE_NOTIFICATION_PROVIDER must be local or oci")
	}
	if err != nil {
		return NotificationWorkerConfig{}, errors.New("notification SMTP configuration is invalid")
	}
	return configuration, nil
}
