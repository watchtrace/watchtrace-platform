package main

import "testing"

func TestParseArgumentsSupportsPhaseOneSQSOnlyVerification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		arguments []string
		wantScope string
		wantPath  string
		wantError bool
	}{
		{arguments: []string{"manifest.json"}, wantScope: "all", wantPath: "manifest.json"},
		{arguments: []string{"-scope", "sqs", "manifest.json"}, wantScope: "sqs", wantPath: "manifest.json"},
		{arguments: []string{"-scope", "iam", "manifest.json"}, wantError: true},
		{arguments: []string{"-scope", "sqs"}, wantError: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.wantScope+test.wantPath, func(t *testing.T) {
			t.Parallel()
			scope, path, err := parseArguments(test.arguments)
			if (err != nil) != test.wantError {
				t.Fatalf("error = %v, wantError %t", err, test.wantError)
			}
			if !test.wantError && (scope != test.wantScope || path != test.wantPath) {
				t.Fatalf("scope/path = %q/%q, want %q/%q", scope, path, test.wantScope, test.wantPath)
			}
		})
	}
}
