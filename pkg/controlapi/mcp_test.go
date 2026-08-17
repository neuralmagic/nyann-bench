package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const mcpScenario = `{"load":{"mode":"concurrent","concurrency":10,"duration":"10s"},"workload":{"type":"faker","isl":128,"osl":64,"turns":1}}`

func TestMCPPlanUsesLogicalTargetAndCLIParityWithoutMutation(t *testing.T) {
	root := t.TempDir()
	client := fake.NewSimpleClientset()
	server := NewServer(client, "test", mcpTestOptions(root))
	input := map[string]any{"target": "kimi-k3", "scenario": json.RawMessage(mcpScenario), "workers": 3, "cpu": "2", "memory": "1Gi", "result_label": "smoke", "vdp_workstream": "env/kimi-bringup"}
	plan, err := server.planBenchmark(decodeInput(t, input))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Target.Name != "kimi-k3" || plan.Target.Model != "mgoin/Kimi-K3-pruned75" {
		t.Fatalf("unexpected logical target: %+v", plan.Target)
	}
	if len(plan.Load) != 1 || fmt.Sprint(plan.Load[0].PerWorkerConcurrency) != "[4 3 3]" {
		t.Fatalf("load division = %+v", plan.Load)
	}
	if plan.Resources.TotalCPU != "6" || plan.Resources.TotalMemory != "3Gi" || plan.Resources.GPURequested || plan.Resources.Queue != "none (CPU Indexed Job)" {
		t.Fatalf("resources = %+v", plan.Resources)
	}
	if plan.job.Spec.CompletionMode == nil || *plan.job.Spec.CompletionMode != batchv1.IndexedCompletion || plan.job.Spec.Template.Spec.AutomountServiceAccountToken == nil || *plan.job.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatalf("rendered job lost Indexed/CPU safety: %+v", plan.job.Spec)
	}
	if len(client.Actions()) != 0 {
		t.Fatalf("plan mutated Kubernetes: %+v", client.Actions())
	}
	payload, _ := json.Marshal(plan)
	if bytes.Contains(payload, []byte("http://kimi-k3-api")) {
		t.Fatalf("plan leaked operator-owned target URL: %s", payload)
	}
}

func TestMCPSubmitIsIdempotentAndRestartRecoverable(t *testing.T) {
	root := t.TempDir()
	client := fake.NewSimpleClientset()
	options := mcpTestOptions(root)
	server := NewServer(client, "test", options)
	input := decodeInput(t, map[string]any{"target": "kimi-k3", "scenario": json.RawMessage(mcpScenario), "workers": 2, "result_label": "sustained", "vdp_workstream": "ws-17"})
	plan, err := server.planBenchmark(input)
	if err != nil {
		t.Fatal(err)
	}
	run, created, err := server.submitBenchmark(context.Background(), plan)
	if err != nil || !created {
		t.Fatalf("first submit: run=%+v created=%v err=%v", run, created, err)
	}
	secondPlan, err := server.planBenchmark(input)
	if err != nil {
		t.Fatal(err)
	}
	second, created, err := server.submitBenchmark(context.Background(), secondPlan)
	if err != nil || created || second.ID != run.ID {
		t.Fatalf("idempotent submit: run=%+v created=%v err=%v", second, created, err)
	}
	job, err := client.BatchV1().Jobs("test").Get(context.Background(), run.ID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if job.Annotations[workstreamAnnotation] != "ws-17" || job.Annotations[mcpManagedAnnotation] != "true" {
		t.Fatalf("recovery annotations missing: %+v", job.Annotations)
	}
	if _, err := NewServer(client, "test", options).getManagedJob(context.Background(), run.ID); err != nil {
		t.Fatalf("new server could not reconstruct run: %v", err)
	}
}

func TestMCPReportAggregatesWorkersAndReportsPartialFailure(t *testing.T) {
	root := t.TempDir()
	client := fake.NewSimpleClientset()
	options := mcpTestOptions(root)
	server := NewServer(client, "test", options)
	plan, err := server.planBenchmark(decodeInput(t, map[string]any{"target": "kimi-k3", "scenario": json.RawMessage(mcpScenario), "workers": 3, "result_label": "partial"}))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := server.submitBenchmark(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	dir := run.Results.Path
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	record0 := `{"id":"0","stream":0,"conv_id":"a","turn":0,"t0":100.1,"ttft_ms":10,"tend":100.5,"prompt_tokens":100,"output_tokens":20,"latency_ms":400,"status":"ok","eval_correct":true}` + "\n"
	record1 := `{"id":"1","stream":0,"conv_id":"b","turn":0,"t0":100.2,"ttft_ms":20,"tend":100.6,"prompt_tokens":120,"output_tokens":30,"latency_ms":400,"status":"ok","eval_correct":false}` + "\n"
	writeFixture(t, filepath.Join(dir, "requests_0.jsonl"), record0)
	writeFixture(t, filepath.Join(dir, "requests_1.jsonl"), record1)
	for worker := 0; worker < 2; worker++ {
		writeFixture(t, filepath.Join(dir, fmt.Sprintf("timestamps_%d.json", worker)), `{"start_time":100,"rampup_end_time":100,"end_time":101,"rampup_duration_seconds":0,"total_duration_seconds":1}`)
	}
	writeFixture(t, filepath.Join(dir, "prometheus_window.json"), `{"raw":"must-not-escape"}`)
	restarted := NewServer(client, "test", options)
	report, err := restarted.getBenchmarkReport(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary == nil || report.Summary.TotalRequests != 2 || report.Summary.TotalPromptTokens != 220 || report.Summary.TotalOutputTokens != 50 || report.Summary.EvalAccuracy != 0.5 {
		t.Fatalf("aggregate summary = %+v", report.Summary)
	}
	if report.WorkerComplete || fmt.Sprint(report.CompleteWorkers) != "[0 1]" || fmt.Sprint(report.MissingWorkers) != "[2]" {
		t.Fatalf("worker completeness = complete:%v have:%v missing:%v", report.WorkerComplete, report.CompleteWorkers, report.MissingWorkers)
	}
	if report.MeasurementWindow == nil || report.MeasurementWindow.StartUnixSeconds != 100 || report.MeasurementWindow.EndUnixSeconds != 101 {
		t.Fatalf("measurement window = %+v", report.MeasurementWindow)
	}
	encoded, _ := json.Marshal(report)
	if bytes.Contains(encoded, []byte("must-not-escape")) || bytes.Contains(encoded, []byte(`"conv_id":"a"`)) || bytes.Contains(encoded, []byte("http://model")) {
		t.Fatalf("raw artifact payload escaped into MCP report: %s", encoded)
	}
	for _, artifact := range report.Artifacts {
		if len(artifact.SHA256) != 64 {
			t.Fatalf("artifact hash missing: %+v", artifact)
		}
	}
}

func TestMCPProtocolIsStrictStatelessAndBounded(t *testing.T) {
	root := t.TempDir()
	client := fake.NewSimpleClientset()
	handler := NewServer(client, "test", mcpTestOptions(root)).Handler()
	discover := mcpRequest(t, handler, "server/discover", map[string]any{})
	if discover.Code != http.StatusOK || !strings.Contains(discover.Body.String(), `"supportedVersions":["2026-07-28"]`) || !strings.Contains(discover.Body.String(), `"resultType":"complete"`) || !strings.Contains(discover.Body.String(), `"io.modelcontextprotocol/serverInfo"`) {
		t.Fatalf("discovery response = %d %s", discover.Code, discover.Body.String())
	}
	listed := mcpRequest(t, handler, "tools/list", map[string]any{})
	if listed.Code != http.StatusOK || listed.Header().Get("Mcp-Session-Id") != "" {
		t.Fatalf("tools/list status/session = %d/%q: %s", listed.Code, listed.Header().Get("Mcp-Session-Id"), listed.Body.String())
	}
	var listedBody struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	decodeRecorder(t, listed, &listedBody)
	if len(listedBody.Result.Tools) != 7 {
		t.Fatalf("tools = %+v", listedBody.Result.Tools)
	}
	tools := map[string]map[string]any{}
	for _, tool := range listedBody.Result.Tools {
		tools[tool["name"].(string)] = tool
	}
	if tools["plan_benchmark"]["annotations"].(map[string]any)["readOnlyHint"] != true || tools["cancel_benchmark"]["annotations"].(map[string]any)["destructiveHint"] != true {
		t.Fatalf("tool annotations = %+v", tools)
	}
	plan := mcpRequest(t, handler, "tools/call", map[string]any{"name": "plan_benchmark", "arguments": map[string]any{"target": "kimi-k3", "scenario": json.RawMessage(mcpScenario), "workers": 3, "result_label": "smoke"}})
	if plan.Code != http.StatusOK || !strings.Contains(plan.Body.String(), `"isError":false`) {
		t.Fatalf("plan response = %d %s", plan.Code, plan.Body.String())
	}
	unknown := mcpRequest(t, handler, "tools/call", map[string]any{"name": "plan_benchmark", "arguments": map[string]any{"target": "kimi-k3", "target_url": "http://attacker", "scenario": json.RawMessage(mcpScenario), "result_label": "bad"}})
	if !strings.Contains(unknown.Body.String(), `"isError":true`) || !strings.Contains(unknown.Body.String(), "unknown field") {
		t.Fatalf("unknown request field accepted: %s", unknown.Body.String())
	}
	legacy := mcpRequest(t, handler, "initialize", map[string]any{})
	if legacy.Code != http.StatusNotFound || !strings.Contains(legacy.Body.String(), "Method not found") {
		t.Fatalf("legacy fallback accepted: %d %s", legacy.Code, legacy.Body.String())
	}
	unsupported := mcpRequestVersion(t, handler, "server/discover", map[string]any{}, "2025-11-25", nil)
	if unsupported.Code != http.StatusBadRequest || !strings.Contains(unsupported.Body.String(), `"code":-32022`) || !strings.Contains(unsupported.Body.String(), `"supported":["2026-07-28"]`) {
		t.Fatalf("unsupported version = %d %s", unsupported.Code, unsupported.Body.String())
	}
	origin := mcpRequestWithHeaders(t, handler, "tools/list", map[string]any{}, map[string]string{"Origin": "https://attacker.example"})
	if origin.Code != http.StatusForbidden {
		t.Fatalf("origin status = %d", origin.Code)
	}
	get := httptest.NewRecorder()
	getRequest := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	getRequest.Header.Set("Authorization", "Bearer test-token")
	handler.ServeHTTP(get, getRequest)
	if get.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp = %d", get.Code)
	}
}

func TestMCPValidationRejectsPathsTargetsAndReplacement(t *testing.T) {
	root := t.TempDir()
	client := fake.NewSimpleClientset()
	server := NewServer(client, "test", mcpTestOptions(root))
	badPath := strings.Replace(mcpScenario, `"type":"faker"`, `"type":"gsm8k","gsm8k_path":"/etc/passwd"`, 1)
	if _, err := server.planBenchmark(decodeInput(t, map[string]any{"target": "kimi-k3", "scenario": json.RawMessage(badPath), "result_label": "bad-path"})); err == nil || !strings.Contains(err.Error(), "dataset root") {
		t.Fatalf("unsafe dataset path error = %v", err)
	}
	if _, err := server.planBenchmark(decodeInput(t, map[string]any{"target": "http://attacker", "scenario": json.RawMessage(mcpScenario), "result_label": "bad-target"})); err == nil || !strings.Contains(err.Error(), "logical") {
		t.Fatalf("arbitrary URL target error = %v", err)
	}
	first := decodeInput(t, map[string]any{"run_id": "named", "target": "kimi-k3", "scenario": json.RawMessage(mcpScenario), "result_label": "one"})
	plan, _ := server.planBenchmark(first)
	if _, _, err := server.submitBenchmark(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	secondScenario := strings.Replace(mcpScenario, `"concurrency":10`, `"concurrency":11`, 1)
	second, _ := server.planBenchmark(decodeInput(t, map[string]any{"run_id": "named", "target": "kimi-k3", "scenario": json.RawMessage(secondScenario), "result_label": "two"}))
	if _, _, err := server.submitBenchmark(context.Background(), second); err == nil || !strings.Contains(err.Error(), "different validated specification") {
		t.Fatalf("named replacement was not rejected: %v", err)
	}
}

func TestMCPCancelIsIdempotentAndCleansResources(t *testing.T) {
	root := t.TempDir()
	client := fake.NewSimpleClientset()
	server := NewServer(client, "test", mcpTestOptions(root))
	plan, err := server.planBenchmark(decodeInput(t, map[string]any{"target": "kimi-k3", "scenario": json.RawMessage(mcpScenario), "result_label": "cancel"}))
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := server.submitBenchmark(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	first, err := server.cancelBenchmarkMCP(context.Background(), run.ID)
	if err != nil || first.(map[string]any)["already_absent"] != false {
		t.Fatalf("first cancel = %+v, %v", first, err)
	}
	second, err := server.cancelBenchmarkMCP(context.Background(), run.ID)
	if err != nil || second.(map[string]any)["already_absent"] != true {
		t.Fatalf("second cancel = %+v, %v", second, err)
	}
}

func TestMCPListResponseIsBounded(t *testing.T) {
	largeResults, _ := json.Marshal(ResultMetadata{Durable: true, Path: "/results/" + strings.Repeat("x", 24<<10), URI: "pvc://results/large"})
	jobs := make([]*batchv1.Job, 60)
	for i := range jobs {
		jobs[i] = &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("large-%02d", i), Namespace: "test", Labels: map[string]string{managedLabel: "true"}, Annotations: map[string]string{createdAnnotation: "2026-08-17T00:00:00Z", resultsAnnotation: string(largeResults)}}}
	}
	client := fake.NewSimpleClientset()
	for _, job := range jobs {
		if _, err := client.BatchV1().Jobs("test").Create(context.Background(), job, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	handler := NewServer(client, "test", mcpTestOptions(t.TempDir())).Handler()
	response := mcpRequest(t, handler, "tools/call", map[string]any{"name": "list_benchmarks", "arguments": map[string]any{"limit": 100}})
	if response.Body.Len() > mcpMaximumResultBytes+4096 || !strings.Contains(response.Body.String(), "exceeds the 1 MiB") || !strings.Contains(response.Body.String(), `"isError":true`) {
		t.Fatalf("unbounded response (%d bytes): %.500s", response.Body.Len(), response.Body.String())
	}
}

func mcpTestOptions(root string) Options {
	options := testOptions()
	options.InferenceTargets = map[string]InferenceTarget{"kimi-k3": {URL: "http://model/v1", Model: "mgoin/Kimi-K3-pruned75"}}
	options.ResultPVC = "results"
	options.ResultRoot = root
	options.DatasetPVC = "benchmark-datasets"
	options.DatasetRoot = filepath.Join(root, "datasets")
	options.AllowedPlatforms = []string{"kubernetes", "openshift"}
	return options
}

func decodeInput(t *testing.T, value any) benchmarkInput {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var input benchmarkInput
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	return input
}

func writeFixture(t *testing.T, name, value string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mcpRequest(t *testing.T, handler http.Handler, method string, params map[string]any) *httptest.ResponseRecorder {
	return mcpRequestWithHeaders(t, handler, method, params, nil)
}

func mcpRequestWithHeaders(t *testing.T, handler http.Handler, method string, params map[string]any, extra map[string]string) *httptest.ResponseRecorder {
	return mcpRequestVersion(t, handler, method, params, mcpProtocolVersion, extra)
}

func mcpRequestVersion(t *testing.T, handler http.Handler, method string, params map[string]any, version string, extra map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": version, "io.modelcontextprotocol/clientInfo": map[string]string{"name": "test", "version": "1"}, "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", version)
	req.Header.Set("Mcp-Method", method)
	if name, ok := params["name"].(string); ok && method == "tools/call" {
		req.Header.Set("Mcp-Name", name)
	}
	for name, value := range extra {
		req.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

func decodeRecorder(t *testing.T, recorder *httptest.ResponseRecorder, value any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), value); err != nil {
		t.Fatalf("decoding %s: %v", recorder.Body.String(), err)
	}
}
