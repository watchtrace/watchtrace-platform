package ownership

import (
	"strings"
	"testing"
)

func TestNormalizeInput(t *testing.T) {
	input, err := normalizeInput("user-id", CreateDefaultInput{
		OrganizationName:   " Example Organization ",
		OrganizationSlug:   " Example-Slug ",
		ProjectName:        " WatchTrace API ",
		ProjectDescription: " Initial monitoring project ",
	})
	if err != nil {
		t.Fatalf("normalize ownership input: %v", err)
	}
	if input.OrganizationName != "Example Organization" || input.OrganizationSlug != "example-slug" {
		t.Fatalf("unexpected organization normalization: %+v", input)
	}
	if input.ProjectName != "WatchTrace API" || input.ProjectDescription != "Initial monitoring project" {
		t.Fatalf("unexpected project normalization: %+v", input)
	}
}

func TestNormalizeInputRejectsInvalidValues(t *testing.T) {
	valid := CreateDefaultInput{
		OrganizationName: "Example",
		OrganizationSlug: "example",
		ProjectName:      "API",
	}

	tests := []struct {
		name   string
		userID string
		input  CreateDefaultInput
	}{
		{name: "missing user", input: valid},
		{name: "missing organization name", userID: "user-id", input: CreateDefaultInput{OrganizationSlug: "example", ProjectName: "API"}},
		{name: "invalid slug", userID: "user-id", input: CreateDefaultInput{OrganizationName: "Example", OrganizationSlug: "invalid_slug", ProjectName: "API"}},
		{name: "leading hyphen", userID: "user-id", input: CreateDefaultInput{OrganizationName: "Example", OrganizationSlug: "-example", ProjectName: "API"}},
		{name: "missing project", userID: "user-id", input: CreateDefaultInput{OrganizationName: "Example", OrganizationSlug: "example"}},
		{name: "long name", userID: "user-id", input: CreateDefaultInput{OrganizationName: strings.Repeat("a", maximumNameBytes+1), OrganizationSlug: "example", ProjectName: "API"}},
		{name: "long description", userID: "user-id", input: CreateDefaultInput{OrganizationName: "Example", OrganizationSlug: "example", ProjectName: "API", ProjectDescription: strings.Repeat("a", maximumDescriptionBytes+1)}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := normalizeInput(test.userID, test.input); err != ErrInvalidInput {
				t.Fatalf("normalization error = %v, want ErrInvalidInput", err)
			}
		})
	}
}
