package notification

import (
	"errors"
	"testing"
	"time"
)

func TestRetrySchedule(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{{1, time.Minute}, {2, 5 * time.Minute}, {3, 25 * time.Minute}, {4, 0}}
	for _, test := range tests {
		if got := retryDelay(test.attempt); got != test.want {
			t.Errorf("attempt %d delay=%s want=%s", test.attempt, got, test.want)
		}
	}
}

func TestProviderStatusIsBoundedAndGeneric(t *testing.T) {
	if got := safeProviderStatus(errors.New("recipient@example.test secret response")); got != "provider_error" {
		t.Fatalf("generic status=%q", got)
	}
	if got := safeProviderStatus(ProviderFailure{Status: "temporary\r\nprovider failure"}); got != "temporaryprovider failure" {
		t.Fatalf("safe status=%q", got)
	}
}

func TestSMTPProviderConfiguration(t *testing.T) {
	if _, err := NewLocalSMTPProvider("127.0.0.1:1025", "watchtrace@localhost"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocalSMTPProvider("smtp.example.test:1025", "watchtrace@example.test"); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatal("local adapter accepted a remote SMTP address")
	}
	if _, err := NewOCIEmailDeliveryProvider("smtp.email.example.test:587", "user", "password", "watchtrace@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewOCIEmailDeliveryProvider("smtp.email.example.test:587", "", "", "watchtrace@example.test"); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatal("OCI adapter accepted missing credentials")
	}
}
