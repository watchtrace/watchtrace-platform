package mail

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

func TestLocalTransportDeliversAndSanitizesHeaders(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	captured := make(chan string, 1)
	serverErrors := make(chan error, 1)
	go servePlainSMTP(listener, captured, serverErrors)

	transport, err := NewLocal(listener.Addr().String(), "watchtrace@localhost", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = transport.Deliver(context.Background(), Message{
		Recipient: "member@example.test", Subject: "Incident\r\nBcc: attacker@example.test",
		MessageID: "delivery-id", PlainTextBody: "first\nsecond\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-serverErrors:
		t.Fatal(err)
	case message := <-captured:
		if strings.Contains(message, "\nBcc:") || !strings.Contains(message, "Subject: IncidentBcc: attacker@example.test") ||
			!strings.Contains(message, "Message-ID: <delivery-id@watchtrace.local>") {
			t.Fatalf("unsafe or incomplete message: %q", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SMTP capture")
	}
}

func TestTransportRejectsUnsafeMailboxAndConfiguration(t *testing.T) {
	if _, err := NewLocal("smtp.example.test:1025", "watchtrace@example.test", time.Second); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatal("local transport accepted a non-loopback host")
	}
	if _, err := NewOCI("smtp.email.example.test:587", "user", "password", "watchtrace@example.test", time.Second); err != nil {
		t.Fatalf("valid OCI configuration rejected: %v", err)
	}
	invalidOCI := []struct{ address, username, password, from string }{
		{"127.0.0.1:587", "user", "password", "watchtrace@example.test"},
		{"localhost:587", "user", "password", "watchtrace@example.test"},
		{"smtp.example.test:587", "", "password", "watchtrace@example.test"},
		{"smtp.example.test:587", "user", "", "watchtrace@example.test"},
		{"smtp.example.test:587", "user\nAUTH PLAIN", "password", "watchtrace@example.test"},
		{"smtp.example.test:587", "user", "password", "watchtrace@example.test\r\nBcc: attacker@example.test"},
	}
	for _, test := range invalidOCI {
		if _, err := NewOCI(test.address, test.username, test.password, test.from, time.Second); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("unsafe OCI configuration accepted: %+v", test)
		}
	}
	transport, err := NewLocal("127.0.0.1:1025", "watchtrace@localhost", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = transport.Deliver(context.Background(), Message{Recipient: "member@example.test\r\nBcc: attacker@example.test", Subject: "safe", PlainTextBody: "body"})
	if stage, ok := StageOf(err); !ok || stage != StageInvalidMessage {
		t.Fatalf("unsafe recipient error=%v stage=%q", err, stage)
	}
}

func TestSTARTTLSFailureIsBoundedAndRedacted(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverErrors := make(chan error, 1)
	go serveSTARTTLSFailure(listener, serverErrors)

	transport := &Transport{
		address: listener.Addr().String(), host: "smtp.example.test", from: "watchtrace@example.test",
		username: "secret-user", password: "secret-password", requireTLS: true, timeout: time.Second,
		tlsConfig: &tls.Config{ServerName: "smtp.example.test", MinVersion: tls.VersionTLS12},
	}
	err = transport.Deliver(context.Background(), Message{Recipient: "member@example.test", Subject: "subject", PlainTextBody: "body"})
	if stage, ok := StageOf(err); !ok || stage != StageSTARTTLS {
		t.Fatalf("STARTTLS error=%v stage=%q", err, stage)
	}
	for _, secret := range []string{"secret-user", "secret-password", "member@example.test", "watchtrace@example.test"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("delivery error exposed %q: %v", secret, err)
		}
	}
	select {
	case serverErr := <-serverErrors:
		if serverErr != nil {
			t.Fatal(serverErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for STARTTLS server")
	}
}

func TestAuthenticationFailureHasDedicatedStage(t *testing.T) {
	certificate, roots := testCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverErrors := make(chan error, 1)
	go serveAuthenticationFailure(listener, certificate, serverErrors)

	transport := &Transport{
		address: listener.Addr().String(), host: "smtp.example.test", from: "watchtrace@example.test",
		username: "user", password: "password", requireTLS: true, timeout: 2 * time.Second,
		tlsConfig: &tls.Config{ServerName: "smtp.example.test", RootCAs: roots, MinVersion: tls.VersionTLS12},
	}
	err = transport.Deliver(context.Background(), Message{Recipient: "member@example.test", Subject: "subject", PlainTextBody: "body"})
	if stage, ok := StageOf(err); !ok || stage != StageAuthentication {
		t.Fatalf("authentication error=%v stage=%q", err, stage)
	}
	select {
	case serverErr := <-serverErrors:
		if serverErr != nil {
			t.Fatal(serverErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for authentication server")
	}
}

func servePlainSMTP(listener net.Listener, captured chan<- string, failures chan<- error) {
	connection, err := listener.Accept()
	if err != nil {
		failures <- err
		return
	}
	defer connection.Close()
	reader, writer := bufio.NewReader(connection), bufio.NewWriter(connection)
	if err = writeSMTPLine(writer, "220 local.test ESMTP"); err != nil {
		failures <- err
		return
	}
	var message strings.Builder
	inData := false
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			failures <- readErr
			return
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if inData {
			if line == "." {
				inData = false
				captured <- message.String()
				if err = writeSMTPLine(writer, "250 accepted"); err != nil {
					failures <- err
					return
				}
				continue
			}
			message.WriteString(line)
			message.WriteByte('\n')
			continue
		}
		switch {
		case strings.HasPrefix(strings.ToUpper(line), "EHLO"):
			err = writeSMTPLine(writer, "250 local.test")
		case strings.HasPrefix(strings.ToUpper(line), "MAIL FROM"), strings.HasPrefix(strings.ToUpper(line), "RCPT TO"):
			err = writeSMTPLine(writer, "250 ok")
		case strings.EqualFold(line, "DATA"):
			inData = true
			err = writeSMTPLine(writer, "354 send data")
		case strings.EqualFold(line, "QUIT"):
			if err = writeSMTPLine(writer, "221 bye"); err != nil {
				failures <- err
			}
			return
		default:
			err = fmt.Errorf("unexpected SMTP command %q", line)
		}
		if err != nil {
			failures <- err
			return
		}
	}
}

func serveSTARTTLSFailure(listener net.Listener, failures chan<- error) {
	connection, err := listener.Accept()
	if err != nil {
		failures <- err
		return
	}
	defer connection.Close()
	reader, writer := bufio.NewReader(connection), bufio.NewWriter(connection)
	if err = writeSMTPLine(writer, "220 local.test ESMTP"); err == nil {
		_, err = reader.ReadString('\n')
	}
	if err == nil {
		err = writeSMTPMultiline(writer, "250-local.test", "250 STARTTLS")
	}
	if err == nil {
		_, err = reader.ReadString('\n')
	}
	if err == nil {
		err = writeSMTPLine(writer, "454 TLS unavailable")
	}
	failures <- err
}

func serveAuthenticationFailure(listener net.Listener, certificate tls.Certificate, failures chan<- error) {
	connection, err := listener.Accept()
	if err != nil {
		failures <- err
		return
	}
	defer connection.Close()
	reader, writer := bufio.NewReader(connection), bufio.NewWriter(connection)
	if err = writeSMTPLine(writer, "220 local.test ESMTP"); err == nil {
		_, err = reader.ReadString('\n')
	}
	if err == nil {
		err = writeSMTPMultiline(writer, "250-local.test", "250 STARTTLS")
	}
	if err == nil {
		_, err = reader.ReadString('\n')
	}
	if err == nil {
		err = writeSMTPLine(writer, "220 begin TLS")
	}
	if err != nil {
		failures <- err
		return
	}
	tlsConnection := tls.Server(connection, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	if err = tlsConnection.Handshake(); err != nil {
		failures <- err
		return
	}
	reader, writer = bufio.NewReader(tlsConnection), bufio.NewWriter(tlsConnection)
	if _, err = reader.ReadString('\n'); err == nil {
		err = writeSMTPMultiline(writer, "250-local.test", "250 AUTH PLAIN")
	}
	if err == nil {
		_, err = reader.ReadString('\n')
	}
	if err == nil {
		err = writeSMTPLine(writer, "535 authentication failed")
	}
	failures <- err
}

func writeSMTPLine(writer *bufio.Writer, line string) error {
	if _, err := fmt.Fprintf(writer, "%s\r\n", line); err != nil {
		return err
	}
	return writer.Flush()
}

func writeSMTPMultiline(writer *bufio.Writer, lines ...string) error {
	for _, line := range lines {
		if _, err := fmt.Fprintf(writer, "%s\r\n", line); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "smtp.example.test"},
		DNSNames: []string{"smtp.example.test"}, NotBefore: time.Now().Add(-time.Minute),
		NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: mustPKCS8(t, privateKey)}),
	)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return certificate, roots
}

func mustPKCS8(t *testing.T, key ed25519.PrivateKey) []byte {
	t.Helper()
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
