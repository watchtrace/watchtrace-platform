package auth

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/url"
	"strings"
	"time"
)

const (
	verificationTokenPrefix  = "wt_verify_"
	passwordResetTokenPrefix = "wt_reset_"
	invitationTokenPrefix    = "wt_invite_"
	verificationSendTimeout  = 5 * time.Second
)

// AccountActionSender delivers raw account-action tokens without persisting
// or logging them.
type AccountActionSender interface {
	SendVerification(context.Context, string, string) error
	SendPasswordReset(context.Context, string, string) error
	SendInvitation(context.Context, string, string) error
}

// LocalSMTPSender delivers development account mail to a loopback SMTP capture
// service such as Mailpit.
type LocalSMTPSender struct {
	*smtpActionSender
}

// OCIEmailDeliverySender delivers account-action mail through OCI Email
// Delivery using authenticated STARTTLS.
type OCIEmailDeliverySender struct {
	*smtpActionSender
}

type smtpActionSender struct {
	address   string
	host      string
	from      string
	baseURL   *url.URL
	resetURL  *url.URL
	inviteURL *url.URL
	username  string
	password  string
	startTLS  bool
}

// NewLocalSMTPSender constructs a local-only plaintext SMTP adapter. Both the
// SMTP server and verification link must use loopback hosts so this adapter
// cannot accidentally become a production mail path.
func NewLocalSMTPSender(address, from, baseURL, resetURL, inviteURL string) (*LocalSMTPSender, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil || !isLoopbackHost(host) {
		return nil, errors.New("verification SMTP address must be a loopback host:port")
	}
	if strings.TrimSpace(from) == "" || strings.ContainsAny(from, "\r\n") {
		return nil, errors.New("verification sender address is invalid")
	}
	parsed, err := parseLocalActionURL(baseURL)
	if err != nil {
		return nil, errors.New("verification URL must be an absolute loopback HTTP URL without query or fragment")
	}
	parsedReset, err := parseLocalActionURL(resetURL)
	if err != nil {
		return nil, errors.New("password-reset URL must be an absolute loopback HTTP URL without query or fragment")
	}
	parsedInvite, err := parseLocalActionURL(inviteURL)
	if err != nil {
		return nil, errors.New("invitation URL must be an absolute loopback HTTP URL without query or fragment")
	}
	return &LocalSMTPSender{smtpActionSender: &smtpActionSender{
		address: address, host: host, from: from, baseURL: parsed, resetURL: parsedReset, inviteURL: parsedInvite,
	}}, nil
}

// NewOCIEmailDeliverySender constructs the production account-action adapter.
// Action links must use HTTPS and credentials never enter a message or error.
func NewOCIEmailDeliverySender(address, username, password, from, baseURL, resetURL, inviteURL string) (*OCIEmailDeliverySender, error) {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil || net.ParseIP(host) != nil || strings.TrimSpace(username) == "" ||
		strings.TrimSpace(password) == "" || strings.TrimSpace(from) == "" || strings.ContainsAny(from, "\r\n") {
		return nil, errors.New("OCI verification SMTP configuration is invalid")
	}
	parsed, err := parseHTTPSActionURL(baseURL)
	if err != nil {
		return nil, errors.New("verification URL must be an absolute HTTPS URL without query or fragment")
	}
	parsedReset, err := parseHTTPSActionURL(resetURL)
	if err != nil {
		return nil, errors.New("password-reset URL must be an absolute HTTPS URL without query or fragment")
	}
	parsedInvite, err := parseHTTPSActionURL(inviteURL)
	if err != nil {
		return nil, errors.New("invitation URL must be an absolute HTTPS URL without query or fragment")
	}
	return &OCIEmailDeliverySender{smtpActionSender: &smtpActionSender{
		address: strings.TrimSpace(address), host: host, from: strings.TrimSpace(from),
		baseURL: parsed, resetURL: parsedReset, inviteURL: parsedInvite,
		username: strings.TrimSpace(username), password: strings.TrimSpace(password), startTLS: true,
	}}, nil
}

func parseLocalActionURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" ||
		!isLoopbackHost(parsed.Hostname()) || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid local action URL")
	}
	return parsed, nil
}

func parseHTTPSActionURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid HTTPS action URL")
	}
	return parsed, nil
}

func (sender *smtpActionSender) SendVerification(ctx context.Context, recipient, token string) error {
	if sender == nil || strings.ContainsAny(recipient, "\r\n") || !validVerificationToken(token) {
		return errors.New("invalid verification delivery input")
	}

	return sender.send(ctx, recipient, sender.message(recipient, token, sender.baseURL,
		"Verify your WatchTrace email", "Verify your WatchTrace email within 24 hours:"))
}

func (sender *smtpActionSender) SendPasswordReset(ctx context.Context, recipient, token string) error {
	if sender == nil || strings.ContainsAny(recipient, "\r\n") || !validPasswordResetToken(token) {
		return errors.New("invalid password-reset delivery input")
	}
	return sender.send(ctx, recipient, sender.message(recipient, token, sender.resetURL,
		"Reset your WatchTrace password", "Reset your WatchTrace password within 1 hour:"))
}

func (sender *smtpActionSender) SendInvitation(ctx context.Context, recipient, token string) error {
	if sender == nil || strings.ContainsAny(recipient, "\r\n") || !ValidInvitationToken(token) {
		return errors.New("invalid invitation delivery input")
	}
	return sender.send(ctx, recipient, sender.message(recipient, token, sender.inviteURL,
		"Join a WatchTrace organization", "Accept this WatchTrace invitation within 7 days:"))
}

func (sender *smtpActionSender) send(ctx context.Context, recipient, message string) error {
	deliveryCtx, cancel := context.WithTimeout(ctx, verificationSendTimeout)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(deliveryCtx, "tcp", sender.address)
	if err != nil {
		return fmt.Errorf("connect to verification SMTP: %w", err)
	}
	defer connection.Close()
	deadline := time.Now().Add(verificationSendTimeout)
	if contextDeadline, ok := deliveryCtx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return fmt.Errorf("set verification SMTP deadline: %w", err)
	}

	host, _, _ := net.SplitHostPort(sender.address)
	client, err := smtp.NewClient(connection, host)
	if err != nil {
		return fmt.Errorf("start verification SMTP: %w", err)
	}
	defer client.Close()
	if sender.startTLS {
		if err = client.StartTLS(&tls.Config{ServerName: sender.host, MinVersion: tls.VersionTLS12}); err != nil {
			return errors.New("start verification SMTP TLS")
		}
		if err = client.Auth(smtp.PlainAuth("", sender.username, sender.password, sender.host)); err != nil {
			return errors.New("authenticate verification SMTP")
		}
	}
	if err := client.Mail(sender.from); err != nil {
		return fmt.Errorf("set verification sender: %w", err)
	}
	if err := client.Rcpt(recipient); err != nil {
		return fmt.Errorf("set verification recipient: %w", err)
	}
	messageWriter, err := client.Data()
	if err != nil {
		return fmt.Errorf("start verification message: %w", err)
	}
	if _, err := io.Copy(messageWriter, strings.NewReader(message)); err != nil {
		_ = messageWriter.Close()
		return fmt.Errorf("write verification message: %w", err)
	}
	if err := messageWriter.Close(); err != nil {
		return fmt.Errorf("finish verification message: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("finish local verification SMTP: %w", err)
	}
	return nil
}

func (sender *smtpActionSender) message(recipient, token string, baseURL *url.URL, subject, instruction string) string {
	actionURL := *baseURL
	query := actionURL.Query()
	query.Set("token", token)
	actionURL.RawQuery = query.Encode()

	var message strings.Builder
	writer := bufio.NewWriter(&message)
	fmt.Fprintf(writer, "From: %s\r\n", sender.from)
	fmt.Fprintf(writer, "To: %s\r\n", recipient)
	fmt.Fprintf(writer, "Subject: %s\r\n", subject)
	fmt.Fprint(writer, "Content-Type: text/plain; charset=UTF-8\r\n")
	fmt.Fprint(writer, "\r\n")
	fmt.Fprintf(writer, "%s\r\n", instruction)
	fmt.Fprintf(writer, "%s\r\n", actionURL.String())
	fmt.Fprint(writer, "\r\nIf you did not request this action, ignore this message.\r\n")
	_ = writer.Flush()
	return message.String()
}

func newVerificationToken() (string, []byte, error) {
	return newOpaqueTokenFrom(rand.Reader, verificationTokenPrefix)
}

func validVerificationToken(token string) bool {
	return validOpaqueToken(token, verificationTokenPrefix)
}

func newPasswordResetToken() (string, []byte, error) {
	return newOpaqueTokenFrom(rand.Reader, passwordResetTokenPrefix)
}

func validPasswordResetToken(token string) bool {
	return validOpaqueToken(token, passwordResetTokenPrefix)
}

// NewInvitationToken creates a purpose-separated raw token and digest for the
// ownership package. Only the digest belongs in PostgreSQL.
func NewInvitationToken() (string, []byte, error) {
	return newOpaqueTokenFrom(rand.Reader, invitationTokenPrefix)
}

func ValidInvitationToken(token string) bool {
	return validOpaqueToken(token, invitationTokenPrefix)
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
