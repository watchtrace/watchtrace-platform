package backendapi

import (
	"encoding/base64"
	"strings"
	"time"

	"github.com/google/uuid"
)

func encodeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}
func decodeCursor(value string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return time.Time{}, "", err
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 2 || len(parts[1]) != 36 {
		return time.Time{}, "", ErrInvalidQuery
	}
	if _, err = uuid.Parse(parts[1]); err != nil {
		return time.Time{}, "", ErrInvalidQuery
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	return at, parts[1], err
}
func nullableTime(v time.Time) any {
	if v.IsZero() {
		return nil
	}
	return v
}
func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}
