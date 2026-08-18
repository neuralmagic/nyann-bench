package controlapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

func TestBearerAuthenticationAndHealth(t *testing.T) {
	handler := NewServer(fake.NewSimpleClientset(), "test", testOptions()).Handler()
	for _, header := range []string{"", "Bearer wrong"} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", header)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("authorization %q status = %d", header, recorder.Code)
		}
	}

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusNoContent {
		t.Fatalf("health status = %d", health.Code)
	}

	unconfigured := DefaultOptions()
	unconfigured.AllowedTargetHosts = []string{"model"}
	recorder := httptest.NewRecorder()
	NewServer(fake.NewSimpleClientset(), "test", unconfigured).Handler().ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodPost, "/mcp", nil),
	)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured auth status = %d", recorder.Code)
	}
}

func TestCommandVectorRESTRoutesDoNotExist(t *testing.T) {
	handler := NewServer(fake.NewSimpleClientset(), "test", testOptions()).Handler()
	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/v1/runs"},
		{http.MethodGet, "/v1/runs"},
		{http.MethodGet, "/v1/runs/example"},
		{http.MethodGet, "/v1/runs/example/logs"},
		{http.MethodDelete, "/v1/runs/example"},
	} {
		response := request(t, handler, test.method, test.path, "")
		if response.Code != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404", test.method, test.path, response.Code)
		}
	}
}

func TestOperatorPolicyValidationRejectsPublicSuffixes(t *testing.T) {
	options := testOptions()
	options.Token = strings.Repeat("t", 32)
	if err := options.Validate(); err != nil {
		t.Fatalf("valid policy: %v", err)
	}
	options.AllowedTargetSuffixes = []string{".example.com"}
	if err := options.Validate(); err == nil {
		t.Fatal("public DNS suffix was accepted")
	}
}

func request(t *testing.T, handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer test-token")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func testOptions() Options {
	options := DefaultOptions()
	options.Token = "test-token"
	options.AllowedTargetHosts = []string{"model"}
	options.AllowedTargetSuffixes = []string{".default.svc"}
	options.AllowedPVCs = []string{"benchmark-results", "benchmark-datasets", "results", "a", "b"}
	options.RunnerImage = "ghcr.io/neuralmagic/nyann-bench@sha256:" + strings.Repeat("a", 64)
	return options
}
