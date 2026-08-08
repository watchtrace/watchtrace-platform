package monitor

import "testing"

const (
	testUserID        = "97867dd1-1283-4477-a9a5-289cac23151a"
	testEnvironmentID = "3249c694-c0fc-4430-95d3-f12419307dd2"
)

func TestNormalizeCreateInputAppliesDefaults(t *testing.T) {
	input, err := normalizeCreateInput(testUserID, testEnvironmentID, CreateInput{
		Name:      " API Health ",
		TargetURL: " https://example.test/health ",
	})
	if err != nil {
		t.Fatalf("normalize monitor: %v", err)
	}
	if input.Name != "API Health" || input.TargetURL != "https://example.test/health" {
		t.Fatalf("unexpected normalized strings: %+v", input)
	}
	if input.IntervalSeconds != 300 || input.TimeoutSeconds != 5 ||
		input.ExpectedStatusMin != 200 || input.ExpectedStatusMax != 299 {
		t.Fatalf("unexpected defaults: %+v", input)
	}
}

func TestNormalizeCreateInputAcceptsDocumentedBounds(t *testing.T) {
	for _, interval := range []int32{60, 120, 300, 600, 1800} {
		input := CreateInput{
			Name:              "API",
			TargetURL:         "custom-scheme://example.test/health",
			IntervalSeconds:   interval,
			TimeoutSeconds:    10,
			ExpectedStatusMin: 201,
			ExpectedStatusMax: 399,
		}
		if _, err := normalizeCreateInput(testUserID, testEnvironmentID, input); err != nil {
			t.Fatalf("interval %d rejected: %v", interval, err)
		}
	}
}

func TestNormalizeCreateInputRejectsInvalidValues(t *testing.T) {
	valid := CreateInput{Name: "API", TargetURL: "https://example.test/health"}
	tests := []struct {
		name  string
		user  string
		env   string
		input CreateInput
	}{
		{name: "missing user", env: testEnvironmentID, input: valid},
		{name: "missing environment", user: testUserID, input: valid},
		{name: "missing name", user: testUserID, env: testEnvironmentID, input: CreateInput{TargetURL: valid.TargetURL}},
		{name: "relative URL", user: testUserID, env: testEnvironmentID, input: CreateInput{Name: "API", TargetURL: "/health"}},
		{name: "URL credentials", user: testUserID, env: testEnvironmentID, input: CreateInput{Name: "API", TargetURL: "https://user:password@example.test"}},
		{name: "URL fragment", user: testUserID, env: testEnvironmentID, input: CreateInput{Name: "API", TargetURL: "https://example.test/#secret"}},
		{name: "unsupported interval", user: testUserID, env: testEnvironmentID, input: CreateInput{Name: "API", TargetURL: valid.TargetURL, IntervalSeconds: 61}},
		{name: "long timeout", user: testUserID, env: testEnvironmentID, input: CreateInput{Name: "API", TargetURL: valid.TargetURL, TimeoutSeconds: 11}},
		{name: "invalid status", user: testUserID, env: testEnvironmentID, input: CreateInput{Name: "API", TargetURL: valid.TargetURL, ExpectedStatusMin: 300, ExpectedStatusMax: 200}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := normalizeCreateInput(test.user, test.env, test.input); err != ErrInvalidInput {
				t.Fatalf("normalization error = %v, want ErrInvalidInput", err)
			}
		})
	}
}
