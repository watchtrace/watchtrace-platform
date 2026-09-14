package httpapi

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/watchtrace/watchtrace-platform/internal/auth"
	"github.com/watchtrace/watchtrace-platform/internal/backendapi"
	"github.com/watchtrace/watchtrace-platform/internal/monitor"
	"github.com/watchtrace/watchtrace-platform/internal/operations"
	"github.com/watchtrace/watchtrace-platform/internal/ownership"
	"github.com/watchtrace/watchtrace-platform/internal/realtime"
)

func TestNewRouterRejectsIncompleteComposition(t *testing.T) {
	tests := []struct {
		name  string
		clear func(*Options)
	}{
		{name: "readiness check", clear: func(options *Options) { options.ReadinessCheck = nil }},
		{name: "auth service", clear: func(options *Options) { options.AuthService = nil }},
		{name: "ownership service", clear: func(options *Options) { options.OwnershipService = nil }},
		{name: "monitor service", clear: func(options *Options) { options.MonitorService = nil }},
		{name: "backend service", clear: func(options *Options) { options.BackendService = nil }},
		{name: "realtime service", clear: func(options *Options) { options.RealtimeService = nil }},
		{name: "operations service", clear: func(options *Options) { options.OperationsService = nil }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := completeRouterOptions()
			test.clear(&options)
			if _, err := NewRouter(options); err == nil || !strings.Contains(err.Error(), test.name) {
				t.Fatalf("error = %v, want missing %s", err, test.name)
			}
		})
	}
}

func TestCustomerAPIRoutesMatchOpenAPI(t *testing.T) {
	router, err := NewRouter(completeRouterOptions())
	if err != nil {
		t.Fatal(err)
	}
	document, err := openapi3.NewLoader().LoadFromFile("../../api/customer-v1.openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}

	expected := make(map[string]struct{})
	for path, item := range document.Paths.Map() {
		for method := range item.Operations() {
			expected[strings.ToUpper(method)+" /api/v1"+path] = struct{}{}
		}
	}
	actual := make(map[string]struct{})
	parameter := regexp.MustCompile(`:([A-Za-z][A-Za-z0-9]*)`)
	for _, route := range router.Routes() {
		if strings.HasPrefix(route.Path, "/api/v1/") {
			path := parameter.ReplaceAllString(route.Path, `{$1}`)
			actual[route.Method+" "+path] = struct{}{}
		}
	}

	for route := range expected {
		if _, ok := actual[route]; !ok {
			t.Errorf("OpenAPI route is not registered: %s", route)
		}
	}
	for route := range actual {
		if _, ok := expected[route]; !ok {
			t.Errorf("registered route is absent from OpenAPI: %s", route)
		}
	}
}

func completeRouterOptions() Options {
	return Options{
		ReadinessCheck:    func(context.Context) error { return nil },
		AuthService:       &auth.Service{},
		OwnershipService:  &ownership.Service{},
		MonitorService:    &monitor.Service{},
		BackendService:    backendapi.New(nil),
		RealtimeService:   realtime.New(nil),
		OperationsService: operations.New(nil),
		RateLimiter:       NewRateLimiter(RateLimits{}),
	}
}
