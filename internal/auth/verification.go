package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	platformmail "github.com/watchtrace/watchtrace-platform/internal/platform/mail"
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
	transport *platformmail.Transport
	baseURL   *url.URL
	resetURL  *url.URL
	inviteURL *url.URL
}

// NewLocalSMTPSender constructs a local-only plaintext SMTP adapter. Both the
// SMTP server and verification link must use loopback hosts so this adapter
// cannot accidentally become a production mail path.
func NewLocalSMTPSender(address, from, baseURL, resetURL, inviteURL string) (*LocalSMTPSender, error) {
	transport, err := platformmail.NewLocal(address, from, verificationSendTimeout)
	if err != nil {
		return nil, errors.New("verification SMTP configuration is invalid")
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
		transport: transport, baseURL: parsed, resetURL: parsedReset, inviteURL: parsedInvite,
	}}, nil
}

// NewOCIEmailDeliverySender constructs the production account-action adapter.
// Action links must use HTTPS and credentials never enter a message or error.
func NewOCIEmailDeliverySender(address, username, password, from, baseURL, resetURL, inviteURL string) (*OCIEmailDeliverySender, error) {
	transport, err := platformmail.NewOCI(address, username, password, from, verificationSendTimeout)
	if err != nil {
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
		transport: transport, baseURL: parsed, resetURL: parsedReset, inviteURL: parsedInvite,
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

	return sender.send(ctx, sender.message(recipient, token, sender.baseURL,
		"Verify your WatchTrace email", "Verify your WatchTrace email within 24 hours:"))
}

func (sender *smtpActionSender) SendPasswordReset(ctx context.Context, recipient, token string) error {
	if sender == nil || strings.ContainsAny(recipient, "\r\n") || !validPasswordResetToken(token) {
		return errors.New("invalid password-reset delivery input")
	}
	return sender.send(ctx, sender.message(recipient, token, sender.resetURL,
		"Reset your WatchTrace password", "Reset your WatchTrace password within 1 hour:"))
}

func (sender *smtpActionSender) SendInvitation(ctx context.Context, recipient, token string) error {
	if sender == nil || strings.ContainsAny(recipient, "\r\n") || !ValidInvitationToken(token) {
		return errors.New("invalid invitation delivery input")
	}
	return sender.send(ctx, sender.message(recipient, token, sender.inviteURL,
		"Join a WatchTrace organization", "Accept this WatchTrace invitation within 7 days:"))
}

func (sender *smtpActionSender) send(ctx context.Context, message platformmail.Message) error {
	if err := sender.transport.Deliver(ctx, message); err != nil {
		return fmt.Errorf("deliver account-action email: %w", err)
	}
	return nil
}

func (sender *smtpActionSender) message(recipient, token string, baseURL *url.URL, subject, instruction string) platformmail.Message {
	actionURL := *baseURL
	query := actionURL.Query()
	query.Set("token", token)
	actionURL.RawQuery = query.Encode()

	return platformmail.Message{
		Recipient: recipient,
		Subject:   subject,
		PlainTextBody: instruction + "\n" + actionURL.String() +
			"\n\nIf you did not request this action, ignore this message.\n",
	}
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
