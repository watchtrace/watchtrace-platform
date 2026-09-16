package fifo

import (
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func databaseTimestamp(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}

func databaseText(value string) pgtype.Text {
	return pgtype.Text{String: value, Valid: true}
}

func optionalInt16(value *int16) pgtype.Int2 {
	if value == nil {
		return pgtype.Int2{}
	}
	return pgtype.Int2{Int16: *value, Valid: true}
}

func optionalInt64(value *int64) pgtype.Int8 {
	if value == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *value, Valid: true}
}

func optionalText(value *string) pgtype.Text {
	if value == nil {
		return pgtype.Text{}
	}
	return databaseText(*value)
}
