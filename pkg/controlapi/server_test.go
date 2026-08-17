package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCreateRunUsesNativeCommandAndCPUIndexedJob(t *testing.T) {
	client := fake.NewSimpleClientset()
	server := NewServer(client, "benchmarks", testOptions())
	server.now = func() time.Time { return time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC) }
	body := `{
      "name":"smoke-run",
      "command":["generate","--target","http://llama-decode.default.svc:8000/v1","--config","{\"load\":{\"concurrency\":4}}"],
      "workers":3,
      "cpu":"2",
      "memory":"4Gi",
      "results":{"pvc":"benchmark-results","mount_path":"/results","subdir":"nightly"}
    }`
	recorder := request(t, server.Handler(), http.MethodPost, "/v1/runs", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var run Run
	decode(t, recorder, &run)
	if run.ID != "smoke-run" || !run.Results.Durable || run.Results.URI != "pvc://benchmark-results/nightly/smoke-run" {
		t.Fatalf("unexpected run: %+v", run)
	}
	if run.ActiveDeadlineSeconds != 3600 || run.TTLSecondsAfterFinished != 86400 {
		t.Fatalf("run limits not exposed: %+v", run)
	}

	job, err := client.BatchV1().Jobs("benchmarks").Get(context.Background(), "smoke-run", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.Spec.CompletionMode == nil || *job.Spec.CompletionMode != batchv1.IndexedCompletion || *job.Spec.Completions != 3 {
		t.Fatalf("job is not a three-worker Indexed Job: %+v", job.Spec)
	}
	if job.Spec.Suspend != nil && *job.Spec.Suspend {
		t.Fatal("CPU benchmark job must not be suspended")
	}
	if len(job.Spec.Template.Spec.InitContainers) != 0 {
		t.Fatal("API-created benchmark Job must not inherit privileged network tuning")
	}
	if job.Spec.Template.Spec.AutomountServiceAccountToken == nil || *job.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("API-created benchmark Job must not mount a service account token")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 3600 {
		t.Fatalf("active deadline = %v", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 86400 {
		t.Fatalf("retention TTL = %v", job.Spec.TTLSecondsAfterFinished)
	}
	if _, ok := job.Labels["kueue.x-k8s.io/queue-name"]; ok {
		t.Fatal("CPU benchmark job must not use Kueue")
	}
	container := job.Spec.Template.Spec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation ||
		container.SecurityContext.RunAsNonRoot == nil || !*container.SecurityContext.RunAsNonRoot {
		t.Fatal("API-created benchmark Job lacks restricted container security context")
	}
	if _, ok := container.Resources.Requests[corev1.ResourceName("nvidia.com/gpu")]; ok {
		t.Fatal("CPU benchmark job must not request a GPU")
	}
	args := strings.Join(container.Args, "\x00")
	for _, expected := range []string{"generate", "--target", "http://llama-decode.default.svc:8000/v1", "--output-dir", "/results/nightly/smoke-run"} {
		if !strings.Contains(args, expected) {
			t.Errorf("container args %q missing %q", container.Args, expected)
		}
	}
	for _, expected := range []string{"--metrics", ":9090", "--workers", "3"} {
		if !strings.Contains(args, expected) {
			t.Errorf("managed container args %q missing %q", container.Args, expected)
		}
	}
	if got := job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName; got != "benchmark-results" {
		t.Fatalf("result PVC = %q", got)
	}
	if _, err := client.CoreV1().Services("benchmarks").Get(context.Background(), "smoke-run", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRunValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"unknown command", `{"command":["analyze","--target","http://model/v1"]}`, "command must start"},
		{"missing target", `{"command":["generate","--config","{}"]}`, "--target must be"},
		{"localhost target", `{"command":["generate","--target","http://localhost:8000/v1"]}`, "reserved"},
		{"owned kube flag", `{"command":["generate","--target","http://model/v1","--kube"]}`, "managed by the API"},
		{"owned workers flag", `{"command":["generate","--target","http://model/v1","--workers","2"]}`, "managed by the API"},
		{"bad name", `{"name":"Bad_Name","command":["generate","--target","http://model/v1"]}`, "DNS-safe"},
		{"too many workers", `{"workers":17,"command":["generate","--target","http://model/v1"]}`, "between 1 and 16"},
		{"bad quantity", `{"cpu":"lots","command":["generate","--target","http://model/v1"]}`, "positive Kubernetes quantity"},
		{"too much cpu", `{"cpu":"17","command":["generate","--target","http://model/v1"]}`, "server maximum"},
		{"too much memory", `{"memory":"65Gi","command":["generate","--target","http://model/v1"]}`, "server maximum"},
		{"public target", `{"command":["generate","--target","https://example.com/v1"]}`, "operator allowlist"},
		{"IP target", `{"command":["generate","--target","http://169.254.169.254/latest"]}`, "IP literals"},
		{"prometheus bypass", `{"command":["generate","--target","http://model/v1","--prometheus-url","http://evil.example"]}`, "not allowed"},
		{"Starlark bypass", `{"command":["generate","--target","http://model/v1","--config","scenario(target=\"http://evil\")"]}`, "inline JSON"},
		{"JSON target bypass", `{"command":["generate","--target","http://model/v1","--config","{\"target\":\"http://evil\"}"]}`, "destination overrides"},
		{"deadline cap", `{"active_deadline_seconds":21601,"command":["generate","--target","http://model/v1"]}`, "active_deadline_seconds"},
		{"retention cap", `{"ttl_seconds_after_finished":604801,"command":["generate","--target","http://model/v1"]}`, "ttl_seconds_after_finished"},
		{"untrusted image", `{"image":"example.com/tool:latest","command":["generate","--target","http://model/v1"]}`, "operator-managed"},
		{"PVC default deny", `{"command":["generate","--target","http://model/v1"],"mounts":[{"pvc":"secret-data","mount_path":"/data"}]}`, "not allowed"},
		{"unsafe result path", `{"command":["generate","--target","http://model/v1"],"results":{"pvc":"results","mount_path":"/data","subdir":"../escape"}}`, "clean relative path"},
		{"duplicate mount path", `{"command":["generate","--target","http://model/v1"],"mounts":[{"pvc":"a","mount_path":"/data"},{"pvc":"b","mount_path":"/data"}]}`, "must be unique"},
		{"unknown JSON field", `{"wat":true,"command":["generate","--target","http://model/v1"]}`, "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := testOptions()
			if tt.name == "PVC default deny" {
				options.AllowedPVCs = nil
			}
			recorder := request(t, NewServer(fake.NewSimpleClientset(), "test", options).Handler(), http.MethodPost, "/v1/runs", tt.body)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), tt.want) {
				t.Fatalf("status/body = %d %s, want %q", recorder.Code, recorder.Body.String(), tt.want)
			}
		})
	}
}

func TestCreateRunAcceptsMultilineInlineJSON(t *testing.T) {
	server := NewServer(fake.NewSimpleClientset(), "test", testOptions())
	body := `{"name":"json","command":["generate","--target","http://model/v1","--config","{\n  \"load\": {\"concurrency\": 4}\n}"]}`
	recorder := request(t, server.Handler(), http.MethodPost, "/v1/runs", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestBearerAuthentication(t *testing.T) {
	handler := NewServer(fake.NewSimpleClientset(), "test", testOptions()).Handler()
	for _, header := range []string{"", "Bearer wrong"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
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
	NewServer(fake.NewSimpleClientset(), "test", unconfigured).Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/runs", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured auth status = %d", recorder.Code)
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

func TestCreateRunIsIdempotentAndRepairsService(t *testing.T) {
	client := fake.NewSimpleClientset()
	server := NewServer(client, "test", testOptions())
	body := `{"name":"same","command":["generate","--target","http://model/v1"]}`
	first := request(t, server.Handler(), http.MethodPost, "/v1/runs", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d: %s", first.Code, first.Body.String())
	}
	if err := client.CoreV1().Services("test").Delete(context.Background(), "same", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	second := request(t, server.Handler(), http.MethodPost, "/v1/runs", body)
	if second.Code != http.StatusOK {
		t.Fatalf("second = %d: %s", second.Code, second.Body.String())
	}
	if _, err := client.CoreV1().Services("test").Get(context.Background(), "same", metav1.GetOptions{}); err != nil {
		t.Fatalf("service was not repaired: %v", err)
	}
	different := request(t, server.Handler(), http.MethodPost, "/v1/runs", `{"name":"same","command":["generate","--target","http://model/v1","--model","other"]}`)
	if different.Code != http.StatusConflict {
		t.Fatalf("different = %d: %s", different.Code, different.Body.String())
	}
}

func TestCreateRunRejectsManagedOrphanServiceWithDifferentFingerprint(t *testing.T) {
	orphan := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "orphan", Namespace: "test",
		Labels:      map[string]string{managedLabel: "true"},
		Annotations: map[string]string{requestAnnotation: "stale"},
	}}
	client := fake.NewSimpleClientset(orphan)
	server := NewServer(client, "test", testOptions())
	recorder := request(t, server.Handler(), http.MethodPost, "/v1/runs", `{"name":"orphan","command":["generate","--target","http://model/v1"]}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	service, err := client.CoreV1().Services("test").Get(context.Background(), "orphan", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if service.Annotations[requestAnnotation] != "stale" {
		t.Fatal("different submission replaced the existing Service")
	}
	if _, err := client.BatchV1().Jobs("test").Get(context.Background(), "orphan", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected Job after conflict: %v", err)
	}
}

func TestRunLifecycleAndLogs(t *testing.T) {
	completed := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "done", Namespace: "test",
			Labels: map[string]string{managedLabel: "true"},
			Annotations: map[string]string{
				commandAnnotation: `["eval","gsm8k","--target","http://model/v1"]`,
				createdAnnotation: "2026-08-05T12:00:00Z",
				resultsAnnotation: `{"durable":false}`,
			},
		},
		Spec: batchv1.JobSpec{Completions: ptr(int32(1))},
		Status: batchv1.JobStatus{Succeeded: 1, Conditions: []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		}}},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "done-abc", Namespace: "test", Labels: map[string]string{"job-name": "done"}}}
	unmanaged := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "test"}}
	client := fake.NewSimpleClientset(completed, pod, unmanaged)
	server := NewServer(client, "test", testOptions())
	server.readLogs = func(_ context.Context, pod string, tail int64) (string, error) {
		if pod != "done-abc" || tail != 42 {
			t.Fatalf("log request = %s/%d", pod, tail)
		}
		return "benchmark complete", nil
	}

	list := request(t, server.Handler(), http.MethodGet, "/v1/runs", "")
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), "other") || !strings.Contains(list.Body.String(), "done") {
		t.Fatalf("unexpected list: %d %s", list.Code, list.Body.String())
	}
	get := request(t, server.Handler(), http.MethodGet, "/v1/runs/done", "")
	var run Run
	decode(t, get, &run)
	if run.Status != "succeeded" || run.Succeeded != 1 {
		t.Fatalf("unexpected run status: %+v", run)
	}
	logs := request(t, server.Handler(), http.MethodGet, "/v1/runs/done/logs?tail_lines=42", "")
	if logs.Code != http.StatusOK || !strings.Contains(logs.Body.String(), "benchmark complete") {
		t.Fatalf("unexpected logs: %d %s", logs.Code, logs.Body.String())
	}
	deleted := request(t, server.Handler(), http.MethodDelete, "/v1/runs/done", "")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d: %s", deleted.Code, deleted.Body.String())
	}
	notFound := request(t, server.Handler(), http.MethodGet, "/v1/runs/done", "")
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("post-delete get = %d", notFound.Code)
	}
	if other := request(t, server.Handler(), http.MethodGet, "/v1/runs/other", ""); other.Code != http.StatusNotFound {
		t.Fatalf("unmanaged Job was exposed: %d", other.Code)
	}
}

func TestDuplicateCreateDoesNotDeleteExistingService(t *testing.T) {
	existing := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "same", Namespace: "test"}}
	client := fake.NewSimpleClientset(existing)
	server := NewServer(client, "test", testOptions())
	body := `{"name":"same","command":["generate","--target","http://model/v1"]}`
	first := request(t, server.Handler(), http.MethodPost, "/v1/runs", body)
	if first.Code != http.StatusConflict {
		t.Fatalf("status = %d: %s", first.Code, first.Body.String())
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
	options.EnableLegacyREST = true
	return options
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder, value any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), value); err != nil {
		t.Fatalf("decoding %q: %v", recorder.Body.String(), err)
	}
}

func ptr[T any](value T) *T { return &value }
