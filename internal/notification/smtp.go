package notification

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"strings"
	"time"
)

const smtpTimeout = 15 * time.Second

type SMTPProvider struct {
	address  string
	host     string
	from     string
	username string
	password string
	startTLS bool
}

// NewLocalSMTPProvider creates the loopback-only development adapter.
func NewLocalSMTPProvider(address, from string) (*SMTPProvider, error) {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil || !isLoopback(host) || !validMailbox(from) {
		return nil, ErrInvalidConfiguration
	}
	return &SMTPProvider{address: strings.TrimSpace(address), host: host, from: strings.TrimSpace(from)}, nil
}

// NewOCIEmailDeliveryProvider creates an authenticated STARTTLS SMTP adapter
// for OCI Email Delivery. Credentials remain process configuration only.
func NewOCIEmailDeliveryProvider(address, username, password, from string) (*SMTPProvider, error) {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil || net.ParseIP(host) != nil || strings.TrimSpace(username) == "" ||
		strings.TrimSpace(password) == "" || !validMailbox(from) {
		return nil, ErrInvalidConfiguration
	}
	return &SMTPProvider{address: strings.TrimSpace(address), host: host, from: strings.TrimSpace(from),
		username: strings.TrimSpace(username), password: strings.TrimSpace(password), startTLS: true}, nil
}

func (provider *SMTPProvider) Send(ctx context.Context, message Message) (ProviderResponse, error) {
	if provider == nil || !validMailbox(message.Recipient) || message.DeliveryID == "" || message.IncidentID == "" {
		return ProviderResponse{}, ProviderFailure{Status: "invalid_message"}
	}
	deliveryCtx, cancel := context.WithTimeout(ctx, smtpTimeout)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(deliveryCtx, "tcp", provider.address)
	if err != nil {
		return ProviderResponse{}, ProviderFailure{Status: "connect_failed"}
	}
	defer connection.Close()
	deadline := time.Now().Add(smtpTimeout)
	if value, ok := deliveryCtx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	if err = connection.SetDeadline(deadline); err != nil {
		return ProviderResponse{}, ProviderFailure{Status: "deadline_failed"}
	}
	client, err := smtp.NewClient(connection, provider.host)
	if err != nil {
		return ProviderResponse{}, ProviderFailure{Status: "smtp_handshake_failed"}
	}
	defer client.Close()
	if provider.startTLS {
		if err = client.StartTLS(&tls.Config{ServerName: provider.host, MinVersion: tls.VersionTLS12}); err != nil {
			return ProviderResponse{}, ProviderFailure{Status: "starttls_failed"}
		}
		if err = client.Auth(smtp.PlainAuth("", provider.username, provider.password, provider.host)); err != nil {
			return ProviderResponse{}, ProviderFailure{Status: "authentication_failed"}
		}
	}
	if err = client.Mail(provider.from); err != nil {
		return ProviderResponse{}, ProviderFailure{Status: "sender_rejected"}
	}
	if err = client.Rcpt(message.Recipient); err != nil {
		return ProviderResponse{}, ProviderFailure{Status: "recipient_rejected"}
	}
	writer, err := client.Data()
	if err != nil {
		return ProviderResponse{}, ProviderFailure{Status: "data_rejected"}
	}
	if _, err = io.Copy(writer, strings.NewReader(provider.message(message))); err != nil {
		_ = writer.Close()
		return ProviderResponse{}, ProviderFailure{Status: "write_failed"}
	}
	if err = writer.Close(); err != nil {
		return ProviderResponse{}, ProviderFailure{Status: "acceptance_failed"}
	}
	if err = client.Quit(); err != nil {
		return ProviderResponse{}, ProviderFailure{Status: "completion_failed"}
	}
	return ProviderResponse{MessageID: message.DeliveryID, Status: "accepted_by_provider"}, nil
}

func (provider *SMTPProvider) message(message Message) string {
	var output strings.Builder
	writer := bufio.NewWriter(&output)
	fmt.Fprintf(writer, "From: %s\r\n", provider.from)
	fmt.Fprintf(writer, "To: %s\r\n", message.Recipient)
	fmt.Fprintf(writer, "Subject: %s\r\n", safeHeader(message.Subject))
	fmt.Fprintf(writer, "Message-ID: <%s@watchtrace.local>\r\n", message.DeliveryID)
	fmt.Fprint(writer, "Content-Type: text/plain; charset=UTF-8\r\n\r\n")
	fmt.Fprint(writer, strings.ReplaceAll(strings.ReplaceAll(message.PlainTextBody, "\r", ""), "\n", "\r\n"))
	_ = writer.Flush()
	return output.String()
}

func validMailbox(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 254 && !strings.ContainsAny(value, "\r\n") && strings.Contains(value, "@")
}

func safeHeader(value string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "").Replace(value))
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
