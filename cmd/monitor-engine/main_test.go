package main

import (
	"errors"
	"strings"
	"testing"
)

func TestSafeErrorRedactsSQSReceiptHandle(t *testing.T) {
	message := safeError(errors.New("api error InvalidParameterValue: Value secret-receipt for parameter ReceiptHandle is invalid. Reason: The receipt handle has expired."))
	if strings.Contains(message, "secret-receipt") || !strings.Contains(message, "[redacted]") || !strings.Contains(message, "expired") {
		t.Fatalf("unsafe error: %s", message)
	}
}
