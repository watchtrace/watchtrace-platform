package contract_test

import (
	"context"
	"os"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/goccy/go-yaml"
)

func TestCustomerOpenAPIContractIsValidAndComplete(t *testing.T) {
	loader := openapi3.NewLoader()
	documentModel, err := loader.LoadFromFile("customer-v1.openapi.yaml")
	if err != nil {
		t.Fatalf("load OpenAPI: %v", err)
	}
	if err = documentModel.Validate(context.Background()); err != nil {
		t.Fatalf("validate OpenAPI: %v", err)
	}
	data, err := os.ReadFile("customer-v1.openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err = yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}
	if document["openapi"] != "3.0.3" {
		t.Fatalf("openapi=%v", document["openapi"])
	}
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatal("paths object is missing")
	}
	required := []string{"/auth/signup", "/auth/me", "/organizations", "/organizations/{orgId}/members/{memberId}", "/organizations/{orgId}/projects", "/projects/{projectId}", "/projects/{projectId}/environments", "/environments/{environmentId}", "/environments/{environmentId}/monitors", "/environments/{environmentId}/monitors/{monitorId}/checks", "/environments/{environmentId}/monitors/{monitorId}/report", "/environments/{environmentId}/dashboard", "/environments/{environmentId}/incidents", "/environments/{environmentId}/incidents/{incidentId}", "/environments/{environmentId}/events"}
	for _, path := range required {
		if _, exists := paths[path]; !exists {
			t.Errorf("missing required path %s", path)
		}
	}
	operationIDs := map[string]string{}
	for path, value := range paths {
		item, ok := value.(map[string]any)
		if !ok {
			t.Errorf("path %s is not an object", path)
			continue
		}
		for method, raw := range item {
			if method == "parameters" {
				continue
			}
			operation, ok := raw.(map[string]any)
			if !ok {
				t.Errorf("%s %s is not an operation", method, path)
				continue
			}
			id, _ := operation["operationId"].(string)
			if id == "" {
				t.Errorf("%s %s has no operationId", method, path)
			} else if previous := operationIDs[id]; previous != "" {
				t.Errorf("duplicate operationId %s on %s and %s", id, previous, path)
			} else {
				operationIDs[id] = path
			}
			if _, ok = operation["responses"].(map[string]any); !ok {
				t.Errorf("%s %s has no responses", method, path)
			}
		}
	}
	components, ok := document["components"].(map[string]any)
	if !ok {
		t.Fatal("components missing")
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok || schemas["Error"] == nil || schemas["Report"] == nil {
		t.Fatal("required error/report schemas missing")
	}
}
