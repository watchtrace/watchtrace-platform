package backendapi

import (
	"errors"
	"testing"
	"time"
)

func TestBoundedQueriesAndStableCursors(t *testing.T) {
	from := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	if _, err := normalizeQuery(PageQuery{Limit: 101, From: from, To: from.Add(time.Hour)}, 24*time.Hour); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("limit error=%v", err)
	}
	if _, err := normalizeQuery(PageQuery{From: from, To: from.Add(25 * time.Hour)}, 24*time.Hour); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("window error=%v", err)
	}
	id := "550e8400-e29b-41d4-a716-446655440000"
	cursor := encodeCursor(from, id)
	at, decoded, err := decodeCursor(cursor)
	if err != nil || !at.Equal(from) || decoded != id {
		t.Fatalf("cursor at=%s id=%s err=%v", at, decoded, err)
	}
	if _, _, err = decodeCursor(encodeCursor(from, "00000000-0000-0000-0000-invalid-value")); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("invalid cursor error=%v", err)
	}
}
