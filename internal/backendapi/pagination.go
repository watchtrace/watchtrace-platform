package backendapi

import (
	"encoding/base64"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
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
func databaseTimestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: !value.IsZero()}
}
func optionalTimestamp(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}
func optionalText(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}
func optionalInt16(value pgtype.Int2) *int16 {
	if !value.Valid {
		return nil
	}
	return &value.Int16
}
func optionalUUID(value pgtype.UUID) *string {
	if !value.Valid {
		return nil
	}
	text := uuid.UUID(value.Bytes).String()
	return &text
}
