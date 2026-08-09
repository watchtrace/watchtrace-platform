package auth

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestVerificationTokensAreOpaqueAndPurposeSpecific(t *testing.T) {
	token, digest, err := newVerificationToken()
	if err != nil {
		t.Fatalf("create verification token: %v", err)
	}
	if !validVerificationToken(token) || validAccessToken(token) || validRefreshToken(token) {
		t.Fatal("verification token was not isolated from session token types")
	}
	if len(digest) != 32 || strings.Contains(string(digest), token) {
		t.Fatal("verification token digest is invalid or contains the raw token")
	}
}

func TestPasswordResetTokensAreOpaqueAndPurposeSpecific(t *testing.T) {
	token, digest, err := newPasswordResetToken()
	if err != nil {
		t.Fatalf("create password-reset token: %v", err)
	}
	if !validPasswordResetToken(token) || validVerificationToken(token) || validAccessToken(token) || validRefreshToken(token) {
		t.Fatal("password-reset token was not isolated from other token types")
	}
	if len(digest) != 32 || strings.Contains(string(digest), token) {
		t.Fatal("password-reset token digest is invalid or contains the raw token")
	}
}

func TestInvitationTokensAreOpaqueAndPurposeSpecific(t *testing.T) {
	token, digest, err := NewInvitationToken()
	if err != nil {
		t.Fatalf("create invitation token: %v", err)
	}
	if !ValidInvitationToken(token) || validVerificationToken(token) || validPasswordResetToken(token) || validAccessToken(token) || validRefreshToken(token) {
		t.Fatal("invitation token was not isolated from other token types")
	}
	if len(digest) != 32 || strings.Contains(string(digest), token) {
		t.Fatal("invitation digest is invalid or contains the raw token")
	}
}

func TestLocalSMTPSenderDeliversVerificationLinkToLoopback(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for test SMTP: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	messages := make(chan string, 1)
	serverErrors := make(chan error, 1)
	go serveTestSMTP(listener, messages, serverErrors)

	sender, err := NewLocalSMTPSender(listener.Addr().String(), "watchtrace@localhost",
		"http://127.0.0.1:3000/verify-email", "http://127.0.0.1:3000/reset-password",
		"http://127.0.0.1:3000/accept-invitation")
	if err != nil {
		t.Fatalf("construct local SMTP sender: %v", err)
	}
	token, _, err := newVerificationToken()
	if err != nil {
		t.Fatalf("create verification token: %v", err)
	}
	if err := sender.SendVerification(context.Background(), "user@example.test", token); err != nil {
		t.Fatalf("send verification: %v", err)
	}

	select {
	case err := <-serverErrors:
		t.Fatalf("test SMTP server: %v", err)
	case message := <-messages:
		if !strings.Contains(message, "To: user@example.test") ||
			!strings.Contains(message, "http://127.0.0.1:3000/verify-email?token="+token) {
			t.Fatalf("verification message is incomplete: %q", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for verification email")
	}
}

func TestLocalSMTPSenderRejectsNonLoopbackConfiguration(t *testing.T) {
	for _, test := range []struct {
		address string
		url     string
	}{
		{address: "smtp.example.test:25", url: "http://127.0.0.1/verify-email"},
		{address: "127.0.0.1:1025", url: "https://example.test/verify-email"},
		{address: "127.0.0.1:1025", url: "http://127.0.0.1/verify-email?token=unsafe"},
	} {
		if _, err := NewLocalSMTPSender(test.address, "watchtrace@localhost", test.url,
			"http://127.0.0.1/reset-password", "http://127.0.0.1/accept-invitation"); err == nil {
			t.Fatalf("unsafe local SMTP configuration accepted: %+v", test)
		}
	}
	if _, err := NewLocalSMTPSender("127.0.0.1:1025", "watchtrace@localhost",
		"http://127.0.0.1/verify-email", "https://example.test/reset-password",
		"http://127.0.0.1/accept-invitation"); err == nil {
		t.Fatal("unsafe password-reset URL was accepted")
	}
	if _, err := NewLocalSMTPSender("127.0.0.1:1025", "watchtrace@localhost",
		"http://127.0.0.1/verify-email", "http://127.0.0.1/reset-password",
		"https://example.test/accept-invitation"); err == nil {
		t.Fatal("unsafe invitation URL was accepted")
	}
}

func serveTestSMTP(listener net.Listener, messages chan<- string, errors chan<- error) {
	connection, err := listener.Accept()
	if err != nil {
		errors <- err
		return
	}
	defer connection.Close()
	if _, err := fmt.Fprint(connection, "220 localhost ESMTP\r\n"); err != nil {
		errors <- err
		return
	}

	scanner := bufio.NewScanner(connection)
	var message strings.Builder
	inData := false
	for scanner.Scan() {
		line := scanner.Text()
		if inData {
			if line == "." {
				inData = false
				if _, err := fmt.Fprint(connection, "250 queued\r\n"); err != nil {
					errors <- err
					return
				}
				continue
			}
			message.WriteString(line)
			message.WriteByte('\n')
			continue
		}

		command := strings.ToUpper(strings.Fields(line)[0])
		switch command {
		case "EHLO", "HELO", "MAIL", "RCPT":
			_, err = fmt.Fprint(connection, "250 ok\r\n")
		case "DATA":
			inData = true
			_, err = fmt.Fprint(connection, "354 end with dot\r\n")
		case "QUIT":
			_, err = fmt.Fprint(connection, "221 bye\r\n")
			if err == nil {
				messages <- message.String()
			}
			return
		default:
			err = fmt.Errorf("unexpected SMTP command %q", line)
		}
		if err != nil {
			errors <- err
			return
		}
	}
	if err := scanner.Err(); err != nil {
		errors <- err
	}
}
