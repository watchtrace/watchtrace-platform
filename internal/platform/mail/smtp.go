// Package mail provides the shared, security-sensitive SMTP transport used by
// account-action and incident-notification email.
package mail

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	mailbox "net/mail"
	"net/smtp"
	"strings"
	"time"
	"unicode"
)

var ErrInvalidConfiguration = errors.New("invalid SMTP configuration")

// FailureStage identifies a bounded SMTP failure without exposing server
// responses, credentials, recipients, or message contents.
type FailureStage string

const (
	StageInvalidMessage FailureStage = "invalid_message"
	StageConnect        FailureStage = "connect"
	StageDeadline       FailureStage = "deadline"
	StageHandshake      FailureStage = "handshake"
	StageSTARTTLS       FailureStage = "starttls"
	StageAuthentication FailureStage = "authentication"
	StageSender         FailureStage = "sender"
	StageRecipient      FailureStage = "recipient"
	StageData           FailureStage = "data"
	StageWrite          FailureStage = "write"
	StageAcceptance     FailureStage = "acceptance"
	StageCompletion     FailureStage = "completion"
)

// DeliveryError intentionally retains only a safe stage. The underlying SMTP
// error can contain provider responses or addresses and must not reach logs.
type DeliveryError struct{ Stage FailureStage }

func (e DeliveryError) Error() string { return "SMTP delivery failed at " + string(e.Stage) }

// StageOf extracts the safe failure stage from a delivery error.
func StageOf(err error) (FailureStage, bool) {
	var deliveryError DeliveryError
	if !errors.As(err, &deliveryError) {
		return "", false
	}
	return deliveryError.Stage, true
}

// Message contains domain-composed content. Header formatting and mailbox
// safety are owned by this package.
type Message struct {
	Recipient     string
	Subject       string
	MessageID     string
	PlainTextBody string
}

// Transport is an SMTP connection configuration. Passwords remain private to
// the process and are never included in returned errors.
type Transport struct {
	address    string
	host       string
	from       string
	username   string
	password   string
	requireTLS bool
	timeout    time.Duration
	tlsConfig  *tls.Config
}

// NewLocal constructs a plaintext transport restricted to loopback SMTP
// capture services such as Mailpit.
func NewLocal(address, from string, timeout time.Duration) (*Transport, error) {
	address, from = strings.TrimSpace(address), strings.TrimSpace(from)
	host, _, err := net.SplitHostPort(address)
	if err != nil || !isLoopback(host) || !validMailbox(from) || timeout <= 0 {
		return nil, ErrInvalidConfiguration
	}
	return &Transport{address: address, host: host, from: from, timeout: timeout}, nil
}

// NewOCI constructs an authenticated STARTTLS transport for OCI Email
// Delivery. TLS 1.2 or newer is always required.
func NewOCI(address, username, password, from string, timeout time.Duration) (*Transport, error) {
	address = strings.TrimSpace(address)
	username, password, from = strings.TrimSpace(username), strings.TrimSpace(password), strings.TrimSpace(from)
	host, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) != nil || isLoopback(host) || !validCredential(username) || !validCredential(password) ||
		!validMailbox(from) || timeout <= 0 {
		return nil, ErrInvalidConfiguration
	}
	return &Transport{
		address: address, host: host, from: from, username: username, password: password,
		requireTLS: true, timeout: timeout,
		tlsConfig: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12},
	}, nil
}

// Deliver sends one plain-text message through the configured transport.
func (transport *Transport) Deliver(ctx context.Context, message Message) error {
	if transport == nil || !validMailbox(message.Recipient) || safeHeader(message.Subject) == "" ||
		!validMessageID(message.MessageID) {
		return DeliveryError{Stage: StageInvalidMessage}
	}
	deliveryCtx, cancel := context.WithTimeout(ctx, transport.timeout)
	defer cancel()

	connection, err := (&net.Dialer{}).DialContext(deliveryCtx, "tcp", transport.address)
	if err != nil {
		return DeliveryError{Stage: StageConnect}
	}
	defer connection.Close()

	deadline := time.Now().Add(transport.timeout)
	if contextDeadline, ok := deliveryCtx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err = connection.SetDeadline(deadline); err != nil {
		return DeliveryError{Stage: StageDeadline}
	}

	client, err := smtp.NewClient(connection, transport.host)
	if err != nil {
		return DeliveryError{Stage: StageHandshake}
	}
	defer client.Close()
	if transport.requireTLS {
		if err = client.StartTLS(transport.tlsConfig.Clone()); err != nil {
			return DeliveryError{Stage: StageSTARTTLS}
		}
		if err = client.Auth(smtp.PlainAuth("", transport.username, transport.password, transport.host)); err != nil {
			return DeliveryError{Stage: StageAuthentication}
		}
	}
	if err = client.Mail(transport.from); err != nil {
		return DeliveryError{Stage: StageSender}
	}
	if err = client.Rcpt(message.Recipient); err != nil {
		return DeliveryError{Stage: StageRecipient}
	}
	writer, err := client.Data()
	if err != nil {
		return DeliveryError{Stage: StageData}
	}
	if _, err = io.Copy(writer, strings.NewReader(transport.format(message))); err != nil {
		_ = writer.Close()
		return DeliveryError{Stage: StageWrite}
	}
	if err = writer.Close(); err != nil {
		return DeliveryError{Stage: StageAcceptance}
	}
	if err = client.Quit(); err != nil {
		return DeliveryError{Stage: StageCompletion}
	}
	return nil
}

func (transport *Transport) format(message Message) string {
	var output strings.Builder
	writer := bufio.NewWriter(&output)
	fmt.Fprintf(writer, "From: %s\r\n", transport.from)
	fmt.Fprintf(writer, "To: %s\r\n", message.Recipient)
	fmt.Fprintf(writer, "Subject: %s\r\n", safeHeader(message.Subject))
	if message.MessageID != "" {
		fmt.Fprintf(writer, "Message-ID: <%s@watchtrace.local>\r\n", message.MessageID)
	}
	fmt.Fprint(writer, "Content-Type: text/plain; charset=UTF-8\r\n\r\n")
	body := strings.ReplaceAll(message.PlainTextBody, "\r", "")
	fmt.Fprint(writer, strings.ReplaceAll(body, "\n", "\r\n"))
	_ = writer.Flush()
	return output.String()
}

func validMailbox(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 254 || strings.ContainsAny(value, "\r\n") {
		return false
	}
	parsed, err := mailbox.ParseAddress(value)
	return err == nil && parsed.Name == "" && parsed.Address == value
}

func validMessageID(value string) bool {
	return len(value) <= 128 && !strings.ContainsAny(value, "\r\n<>")
}

func validCredential(value string) bool {
	return value != "" && len(value) <= 1024 && strings.IndexFunc(value, unicode.IsControl) == -1
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
