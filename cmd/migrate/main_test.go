package main

import (
	"bytes"
	"testing"
)

func TestRunRejectsInvalidArgumentsBeforeLoadingConfiguration(t *testing.T) {
	tests := []struct {
		name      string
		arguments []string
	}{
		{name: "missing action"},
		{name: "unknown action", arguments: []string{"drop"}},
		{name: "extra action", arguments: []string{"up", "down"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := run(test.arguments, &bytes.Buffer{})
			if err == nil {
				t.Fatal("run succeeded, want a usage error")
			}
			if err.Error() != usage {
				t.Fatalf("error = %q, want %q", err, usage)
			}
		})
	}
}
