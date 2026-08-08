package auth

import (
	"bufio"
	"context"
	"crypto/rand"
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
	verificationTokenPrefix = "wt_verify_"
	verificationSendTimeout = 5 * time.Second
)

// VerificationSender delivers one raw verification token without persisting
// or logging it. Production provider integration remains owned by P1-406.
type VerificationSender interface {
	SendVerification(context.Context, string, string) error
}

// LocalSMTPSender delivers development verification mail to a loopback SMTP
// capture service such as Mailpit.
type LocalSMTPSender struct {
	address string
	from    string
	baseURL *url.URL
}

// NewLocalSMTPSender constructs a local-only plaintext SMTP adapter. Both the
// SMTP server and verification link must use loopback hosts so this adapter
// cannot accidentally become a production mail path.
func NewLocalSMTPSender(address, from, baseURL string) (*LocalSMTPSender, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil || !isLoopbackHost(host) {
		return nil, errors.New("verification SMTP address must be a loopback host:port")
	}
	if strings.TrimSpace(from) == "" || strings.ContainsAny(from, "\r\n") {
		return nil, errors.New("verification sender address is invalid")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" ||
		!isLoopbackHost(parsed.Hostname()) || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("verification URL must be an absolute loopback HTTP URL without query or fragment")
	}
	return &LocalSMTPSender{address: address, from: from, baseURL: parsed}, nil
}

func (sender *LocalSMTPSender) SendVerification(ctx context.Context, recipient, token string) error {
	if sender == nil || strings.ContainsAny(recipient, "\r\n") || !validVerificationToken(token) {
		return errors.New("invalid verification delivery input")
	}

	deliveryCtx, cancel := context.WithTimeout(ctx, verificationSendTimeout)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(deliveryCtx, "tcp", sender.address)
	if err != nil {
		return fmt.Errorf("connect to local verification SMTP: %w", err)
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
		return fmt.Errorf("start local verification SMTP: %w", err)
	}
	defer client.Close()
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
	if _, err := io.Copy(messageWriter, strings.NewReader(sender.message(recipient, token))); err != nil {
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

func (sender *LocalSMTPSender) message(recipient, token string) string {
	verificationURL := *sender.baseURL
	query := verificationURL.Query()
	query.Set("token", token)
	verificationURL.RawQuery = query.Encode()

	var message strings.Builder
	writer := bufio.NewWriter(&message)
	fmt.Fprintf(writer, "From: %s\r\n", sender.from)
	fmt.Fprintf(writer, "To: %s\r\n", recipient)
	fmt.Fprint(writer, "Subject: Verify your WatchTrace email\r\n")
	fmt.Fprint(writer, "Content-Type: text/plain; charset=UTF-8\r\n")
	fmt.Fprint(writer, "\r\n")
	fmt.Fprint(writer, "Verify your WatchTrace email within 24 hours:\r\n")
	fmt.Fprintf(writer, "%s\r\n", verificationURL.String())
	fmt.Fprint(writer, "\r\nIf you did not create this account, ignore this message.\r\n")
	_ = writer.Flush()
	return message.String()
}

func newVerificationToken() (string, []byte, error) {
	return newOpaqueTokenFrom(rand.Reader, verificationTokenPrefix)
}

func validVerificationToken(token string) bool {
	return validOpaqueToken(token, verificationTokenPrefix)
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
