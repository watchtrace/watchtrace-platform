package monitor

import (
	"sort"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/watchtrace/watchtrace-platform/internal/secureheaders"
)

func (s *Service) headerNames(ciphertext []byte, version pgtype.Int4) []string {
	if len(ciphertext) == 0 || !version.Valid {
		return []string{}
	}
	headers, err := s.headers.Decrypt(ciphertext, version.Int32)
	if err != nil {
		return []string{}
	}
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func nullableInt32(value int32) any {
	if value == 0 {
		return nil
	}
	return value
}
func (s *Service) encryptHeaders(headers map[string]string) ([]byte, int32, []string, error) {
	if len(headers) == 0 {
		return nil, 0, []string{}, nil
	}
	ciphertext, version, err := s.headers.Encrypt(headers)
	if err != nil {
		return nil, 0, nil, ErrInvalidInput
	}
	normalized, _ := secureheaders.Normalize(headers)
	names := make([]string, 0, len(normalized))
	for name := range normalized {
		names = append(names, name)
	}
	sort.Strings(names)
	return ciphertext, version, names, nil
}
