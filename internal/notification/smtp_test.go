package notification

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestLocalSMTPProviderDeliversStableIdentity(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	captured := make(chan string, 1)
	serverErrors := make(chan error, 1)
	go serveOneSMTP(listener, captured, serverErrors)

	provider, err := NewLocalSMTPProvider(listener.Addr().String(), "watchtrace@localhost")
	if err != nil {
		t.Fatal(err)
	}
	message := Message{DeliveryID: "550e8400-e29b-41d4-a716-446655440001",
		IncidentID: "550e8400-e29b-41d4-a716-446655440002", Recipient: "member@example.test",
		Transition: "opened", Subject: "WatchTrace incident opened",
		PlainTextBody: "Incident ID: 550e8400-e29b-41d4-a716-446655440002\nDelivery ID: 550e8400-e29b-41d4-a716-446655440001\n"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := provider.Send(ctx, message)
	if err != nil {
		t.Fatal(err)
	}
	if response.MessageID != message.DeliveryID || response.Status != "accepted_by_provider" {
		t.Fatalf("provider response=%+v", response)
	}
	select {
	case serverErr := <-serverErrors:
		t.Fatal(serverErr)
	case body := <-captured:
		if !strings.Contains(body, message.IncidentID) || !strings.Contains(body, message.DeliveryID) {
			t.Fatalf("SMTP body omitted stable identity: %q", body)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func serveOneSMTP(listener net.Listener, captured chan<- string, failures chan<- error) {
	connection, err := listener.Accept()
	if err != nil {
		failures <- err
		return
	}
	defer connection.Close()
	reader, writer := bufio.NewReader(connection), bufio.NewWriter(connection)
	write := func(line string) bool {
		if _, writeErr := fmt.Fprintf(writer, "%s\r\n", line); writeErr != nil {
			failures <- writeErr
			return false
		}
		if flushErr := writer.Flush(); flushErr != nil {
			failures <- flushErr
			return false
		}
		return true
	}
	if !write("220 local.test ESMTP") {
		return
	}
	inData := false
	var data strings.Builder
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
				captured <- data.String()
				if !write("250 accepted") {
					return
				}
				continue
			}
			data.WriteString(line)
			data.WriteByte('\n')
			continue
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			if !write("250 local.test") {
				return
			}
		case strings.HasPrefix(upper, "MAIL FROM"), strings.HasPrefix(upper, "RCPT TO"):
			if !write("250 ok") {
				return
			}
		case upper == "DATA":
			inData = true
			if !write("354 end with dot") {
				return
			}
		case upper == "QUIT":
			_ = write("221 bye")
			return
		default:
			if !write("500 unexpected") {
				return
			}
		}
	}
}
