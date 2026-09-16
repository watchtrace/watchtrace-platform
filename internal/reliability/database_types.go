package reliability

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func databaseTimestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

func optionalDatabaseTimestamp(value *time.Time) pgtype.Timestamptz {
	if value == nil {
		return pgtype.Timestamptz{}
	}
	return databaseTimestamp(*value)
}

func optionalDatabaseString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func optionalTime(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
