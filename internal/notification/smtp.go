package notification

import (
	"context"
	"time"

	platformmail "github.com/watchtrace/watchtrace-platform/internal/platform/mail"
)

const smtpTimeout = 15 * time.Second

type SMTPProvider struct {
	transport *platformmail.Transport
}

// NewLocalSMTPProvider creates the loopback-only development adapter.
func NewLocalSMTPProvider(address, from string) (*SMTPProvider, error) {
	transport, err := platformmail.NewLocal(address, from, smtpTimeout)
	if err != nil {
		return nil, ErrInvalidConfiguration
	}
	return &SMTPProvider{transport: transport}, nil
}

// NewOCIEmailDeliveryProvider creates an authenticated STARTTLS SMTP adapter
// for OCI Email Delivery. Credentials remain process configuration only.
func NewOCIEmailDeliveryProvider(address, username, password, from string) (*SMTPProvider, error) {
	transport, err := platformmail.NewOCI(address, username, password, from, smtpTimeout)
	if err != nil {
		return nil, ErrInvalidConfiguration
	}
	return &SMTPProvider{transport: transport}, nil
}

func (provider *SMTPProvider) Send(ctx context.Context, message Message) (ProviderResponse, error) {
	if provider == nil || provider.transport == nil || message.DeliveryID == "" || message.IncidentID == "" {
		return ProviderResponse{}, ProviderFailure{Status: "invalid_message"}
	}
	err := provider.transport.Deliver(ctx, platformmail.Message{
		Recipient: message.Recipient, Subject: message.Subject,
		MessageID: message.DeliveryID, PlainTextBody: message.PlainTextBody,
	})
	if err != nil {
		return ProviderResponse{}, smtpProviderFailure(err)
	}
	return ProviderResponse{MessageID: message.DeliveryID, Status: "accepted_by_provider"}, nil
}

func smtpProviderFailure(err error) ProviderFailure {
	stage, ok := platformmail.StageOf(err)
	if !ok {
		return ProviderFailure{Status: "provider_error"}
	}
	status := "provider_error"
	switch stage {
	case platformmail.StageInvalidMessage:
		status = "invalid_message"
	case platformmail.StageConnect:
		status = "connect_failed"
	case platformmail.StageDeadline:
		status = "deadline_failed"
	case platformmail.StageHandshake:
		status = "smtp_handshake_failed"
	case platformmail.StageSTARTTLS:
		status = "starttls_failed"
	case platformmail.StageAuthentication:
		status = "authentication_failed"
	case platformmail.StageSender:
		status = "sender_rejected"
	case platformmail.StageRecipient:
		status = "recipient_rejected"
	case platformmail.StageData:
		status = "data_rejected"
	case platformmail.StageWrite:
		status = "write_failed"
	case platformmail.StageAcceptance:
		status = "acceptance_failed"
	case platformmail.StageCompletion:
		status = "completion_failed"
	}
	return ProviderFailure{Status: status}
}
