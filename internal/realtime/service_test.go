package realtime

import "testing"

func TestParseLastID(t *testing.T) {
	for _, value := range []string{"-1", "not-a-number", "9223372036854775808"} {
		if _, err := ParseLastID(value); err == nil {
			t.Errorf("accepted cursor %q", value)
		}
	}
	if value, err := ParseLastID("42"); err != nil || value != 42 {
		t.Fatalf("value=%d err=%v", value, err)
	}
}
