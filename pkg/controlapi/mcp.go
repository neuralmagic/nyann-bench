package controlapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/neuralmagic/nyann-bench/pkg/analysis"
	"github.com/neuralmagic/nyann-bench/pkg/config"
	"github.com/neuralmagic/nyann-bench/pkg/kube"
)

const (
	mcpProtocolVersion      = "2026-07-28"
	mcpMaximumRequestBytes  = 1 << 20
	mcpMaximumResultBytes   = 1 << 20
	mcpMaximumScenarioBytes = 64 << 10
	scenarioAnnotation      = "nyann-bench.neuralmagic.com/effective-scenario"
	targetAnnotation        = "nyann-bench.neuralmagic.com/target"
	resultLabelAnnotation   = "nyann-bench.neuralmagic.com/result-label"
	platformAnnotation      = "nyann-bench.neuralmagic.com/platform"
	workstreamAnnotation    = "vdp.neuralmagic.com/workstream"
	mcpManagedAnnotation    = "nyann-bench.neuralmagic.com/mcp-managed"
	mcpServerName           = "nyann-bench"
	mcpServerVersion        = "0.1.0"
)

var (
	resultLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,61}[a-z0-9])?$`)
	attachmentPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	artifactPattern    = regexp.MustCompile(`^(requests_([0-9]+)\.jsonl|timestamps_([0-9]+)\.json|prometheus(?:_[A-Za-z0-9.-]+)?\.json)$`)
)

type benchmarkInput struct {
	RunID          string          `json:"run_id,omitempty"`
	Target         string          `json:"target"`
	Scenario       json.RawMessage `json:"scenario"`
	Workers        int             `json:"workers,omitempty"`
	CPU            string          `json:"cpu,omitempty"`
	Memory         string          `json:"memory,omitempty"`
	Platform       string          `json:"platform,omitempty"`
	Architecture   string          `json:"architecture,omitempty"`
	DeadlineSecond int64           `json:"deadline_seconds,omitempty"`
	ResultLabel    string          `json:"result_label"`
	VDPWorkstream  string          `json:"vdp_workstream,omitempty"`
}

type benchmarkRef struct {
	RunID string `json:"run_id"`
}

type benchmarkListInput struct {
	Status string `json:"status,omitempty"`
	Label  string `json:"result_label,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type plannedBenchmark struct {
	RunID             string          `json:"run_id"`
	Target            plannedTarget   `json:"target"`
	EffectiveScenario any             `json:"effective_scenario"`
	Load              []plannedStage  `json:"load"`
	Resources         resourceSummary `json:"resources"`
	Results           ResultMetadata  `json:"results"`
	DeadlineSeconds   int64           `json:"deadline_seconds"`
	Warnings          []string        `json:"validation_warnings"`
	createRequest     CreateRunRequest
	config            kube.KubeConfig
	command           []string
	service           *corev1.Service
	job               *batchv1.Job
	fingerprint       string
	resultLabel       string
	workstream        string
}

type plannedTarget struct {
	Name  string `json:"name"`
	Model string `json:"model,omitempty"`
}

type plannedStage struct {
	Index                int       `json:"index"`
	Name                 string    `json:"name,omitempty"`
	Mode                 string    `json:"mode"`
	DurationSeconds      float64   `json:"duration_seconds"`
	TotalConcurrency     int       `json:"total_concurrency,omitempty"`
	PerWorkerConcurrency []int     `json:"per_worker_concurrency,omitempty"`
	TotalRate            float64   `json:"total_rate,omitempty"`
	PerWorkerRate        []float64 `json:"per_worker_rate,omitempty"`
}

type resourceSummary struct {
	Platform      string `json:"platform"`
	Architecture  string `json:"architecture"`
	Workers       int    `json:"workers"`
	CPUPerWorker  string `json:"cpu_per_worker"`
	MemoryPerWork string `json:"memory_per_worker"`
	TotalCPU      string `json:"total_cpu"`
	TotalMemory   string `json:"total_memory"`
	GPURequested  bool   `json:"gpu_requested"`
	Queue         string `json:"queue"`
	Image         string `json:"image"`
}

type artifactMetadata struct {
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	Worker     *int      `json:"worker,omitempty"`
	SizeBytes  int64     `json:"size_bytes"`
	SHA256     string    `json:"sha256"`
	ModifiedAt time.Time `json:"modified_at"`
}

type measurementWindow struct {
	StartUnixSeconds float64 `json:"start_unix_seconds"`
	EndUnixSeconds   float64 `json:"end_unix_seconds"`
}

type benchmarkReport struct {
	SchemaVersion     string             `json:"schema_version"`
	Run               Run                `json:"run"`
	Target            string             `json:"target"`
	ResultLabel       string             `json:"result_label,omitempty"`
	VDPWorkstream     string             `json:"vdp_workstream,omitempty"`
	EffectiveScenario json.RawMessage    `json:"effective_scenario,omitempty"`
	Image             string             `json:"image"`
	MeasurementWindow *measurementWindow `json:"measurement_window,omitempty"`
	Summary           *analysis.Summary  `json:"summary,omitempty"`
	CompleteWorkers   []int              `json:"complete_workers"`
	MissingWorkers    []int              `json:"missing_workers"`
	WorkerComplete    bool               `json:"worker_complete"`
	Artifacts         []artifactMetadata `json:"artifacts"`
	Warnings          []string           `json:"warnings"`
}

func (s *Server) MCPHandler() http.Handler {
	return http.HandlerFunc(s.handleMCP)
}

type mcpEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpParams struct {
	Meta      mcpMetadata     `json:"_meta"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Cursor    *string         `json:"cursor,omitempty"`
}

type mcpMetadata struct {
	ProtocolVersion    string          `json:"io.modelcontextprotocol/protocolVersion"`
	ClientInfo         *mcpClientInfo  `json:"io.modelcontextprotocol/clientInfo,omitempty"`
	ClientCapabilities json.RawMessage `json:"io.modelcontextprotocol/clientCapabilities"`
}

type mcpClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" {
		mcpError(w, nil, http.StatusForbidden, -32600, "Origin is not allowed", nil)
		return
	}
	if contentType := strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]); !strings.EqualFold(contentType, "application/json") {
		mcpError(w, nil, http.StatusUnsupportedMediaType, -32600, "Content-Type must be application/json", nil)
		return
	}
	var envelope mcpEnvelope
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, mcpMaximumRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&envelope); err != nil || ensureEOF(dec) != nil {
		mcpError(w, nil, http.StatusBadRequest, -32700, "Parse error", nil)
		return
	}
	if envelope.JSONRPC != "2.0" || !validMCPID(envelope.ID) || envelope.Method == "" {
		mcpError(w, nil, http.StatusBadRequest, -32600, "Invalid Request", nil)
		return
	}
	var params mcpParams
	if err := decodeStrict(envelope.Params, &params); err != nil {
		mcpError(w, envelope.ID, http.StatusBadRequest, -32602, "Invalid params: "+err.Error(), nil)
		return
	}
	if params.Meta.ProtocolVersion == "" || len(params.Meta.ClientCapabilities) == 0 || !jsonObject(params.Meta.ClientCapabilities) || (params.Meta.ClientInfo != nil && (params.Meta.ClientInfo.Name == "" || params.Meta.ClientInfo.Version == "")) {
		mcpError(w, envelope.ID, http.StatusBadRequest, -32602, "Invalid params: required per-request MCP metadata is missing", nil)
		return
	}
	headerName, err := decodeMCPName(r.Header.Get("Mcp-Name"))
	if err != nil || r.Header.Get("Mcp-Protocol-Version") != params.Meta.ProtocolVersion || r.Header.Get("Mcp-Method") != envelope.Method || (envelope.Method == "tools/call" && headerName != params.Name) {
		mcpError(w, envelope.ID, http.StatusBadRequest, -32020, "Header mismatch: MCP request headers do not match the body", nil)
		return
	}
	if params.Meta.ProtocolVersion != mcpProtocolVersion {
		mcpError(w, envelope.ID, http.StatusBadRequest, -32022, "Unsupported protocol version", map[string]any{"supported": []string{mcpProtocolVersion}, "requested": params.Meta.ProtocolVersion})
		return
	}
	switch envelope.Method {
	case "server/discover":
		if params.Name != "" || params.Cursor != nil || len(params.Arguments) != 0 {
			mcpError(w, envelope.ID, http.StatusBadRequest, -32602, "Invalid discovery params", nil)
			return
		}
		mcpResult(w, envelope.ID, map[string]any{"supportedVersions": []string{mcpProtocolVersion}, "capabilities": map[string]any{"tools": map[string]any{}}, "instructions": "Plan before submitting. Use logical targets and bounded reports; raw JSONL and Prometheus payloads are intentionally unavailable through MCP.", "ttlMs": 300000, "cacheScope": "private"})
	case "tools/list":
		if params.Cursor != nil || params.Name != "" || len(params.Arguments) != 0 {
			mcpError(w, envelope.ID, http.StatusBadRequest, -32602, "Invalid params: this tool list is not paginated", nil)
			return
		}
		mcpResult(w, envelope.ID, map[string]any{"tools": s.mcpTools(), "ttlMs": 300000, "cacheScope": "private"})
	case "tools/call":
		if params.Cursor != nil || params.Name == "" || len(params.Arguments) == 0 || !jsonObject(params.Arguments) {
			mcpError(w, envelope.ID, http.StatusBadRequest, -32602, "Invalid tool call", nil)
			return
		}
		if !isMCPTool(params.Name) {
			mcpError(w, envelope.ID, http.StatusBadRequest, -32602, "Unknown tool: "+params.Name, nil)
			return
		}
		value, callErr := s.callMCPTool(r.Context(), params.Name, params.Arguments)
		if callErr != nil {
			mcpToolResult(w, envelope.ID, map[string]any{"error": callErr.Error()}, true)
			return
		}
		mcpToolResult(w, envelope.ID, value, false)
	default:
		mcpError(w, envelope.ID, http.StatusNotFound, -32601, "Method not found: "+envelope.Method, nil)
	}
}

func (s *Server) callMCPTool(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	switch name {
	case "plan_benchmark", "submit_benchmark":
		var input benchmarkInput
		if err := decodeStrict(raw, &input); err != nil {
			return nil, fmt.Errorf("invalid benchmark request: %w", err)
		}
		plan, err := s.planBenchmark(input)
		if err != nil {
			return nil, err
		}
		if name == "plan_benchmark" {
			return plan, nil
		}
		run, created, err := s.submitBenchmark(ctx, plan)
		if err != nil {
			return nil, err
		}
		run.Command = nil
		return map[string]any{"run": run, "created": created, "plan": plan}, nil
	case "list_benchmarks":
		var input benchmarkListInput
		if err := decodeStrict(raw, &input); err != nil {
			return nil, err
		}
		return s.listBenchmarksMCP(ctx, input)
	case "get_benchmark":
		ref, err := decodeBenchmarkRef(raw)
		if err != nil {
			return nil, err
		}
		job, err := s.getManagedJob(ctx, ref.RunID)
		if err != nil {
			return nil, friendlyKubeError(err)
		}
		return benchmarkDetails(job), nil
	case "cancel_benchmark":
		ref, err := decodeBenchmarkRef(raw)
		if err != nil {
			return nil, err
		}
		return s.cancelBenchmarkMCP(ctx, ref.RunID)
	case "list_benchmark_artifacts":
		ref, err := decodeBenchmarkRef(raw)
		if err != nil {
			return nil, err
		}
		job, err := s.getManagedJob(ctx, ref.RunID)
		if err != nil {
			return nil, friendlyKubeError(err)
		}
		artifacts, err := s.listArtifacts(runFromJob(job))
		if err != nil {
			return nil, err
		}
		return map[string]any{"run_id": ref.RunID, "artifacts": artifacts}, nil
	case "get_benchmark_report":
		ref, err := decodeBenchmarkRef(raw)
		if err != nil {
			return nil, err
		}
		return s.getBenchmarkReport(ctx, ref.RunID)
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func (s *Server) planBenchmark(input benchmarkInput) (*plannedBenchmark, error) {
	if len(input.Scenario) == 0 || len(input.Scenario) > mcpMaximumScenarioBytes || !jsonObject(input.Scenario) {
		return nil, fmt.Errorf("scenario must be a JSON object no larger than %d bytes", mcpMaximumScenarioBytes)
	}
	var typed config.Config
	if err := decodeStrict(input.Scenario, &typed); err != nil {
		return nil, fmt.Errorf("invalid typed scenario: %w", err)
	}
	scenario, err := config.Parse(string(input.Scenario))
	if err != nil {
		return nil, fmt.Errorf("invalid typed scenario: %w", err)
	}
	if err := validateScenarioBounds(scenario); err != nil {
		return nil, err
	}
	target, ok := s.options.InferenceTargets[input.Target]
	if !ok || input.Target == "" {
		return nil, fmt.Errorf("target must name an operator-configured logical inference target")
	}
	if input.ResultLabel == "" || !resultLabelPattern.MatchString(input.ResultLabel) {
		return nil, fmt.Errorf("result_label must be a lowercase DNS-safe label of at most 63 characters")
	}
	if input.VDPWorkstream != "" && !attachmentPattern.MatchString(input.VDPWorkstream) {
		return nil, fmt.Errorf("vdp_workstream contains unsupported characters or is too long")
	}
	workers := input.Workers
	if workers == 0 {
		workers = 1
	}
	platform := input.Platform
	if platform == "" {
		platform = "kubernetes"
	}
	if !containsString(s.options.allowedPlatforms(), platform) {
		return nil, fmt.Errorf("platform %q is not allowed by the operator", platform)
	}
	if s.options.ResultPVC == "" || s.options.ResultRoot == "" {
		return nil, fmt.Errorf("durable result storage is not configured")
	}
	if err := validateDatasetPaths(typed.Workload, s.options.DatasetRoot); err != nil {
		return nil, err
	}
	canonical, _ := json.Marshal(input)
	digest := sha256.Sum256(canonical)
	runID := input.RunID
	if runID == "" {
		prefix := strings.ReplaceAll(input.ResultLabel, ".", "-")
		if len(prefix) > 40 {
			prefix = prefix[:40]
		}
		runID = fmt.Sprintf("nyann-%s-%s", strings.Trim(prefix, "-"), hex.EncodeToString(digest[:6]))
	}
	if len(validation.IsDNS1123Subdomain(runID)) > 0 || len(runID) > 63 {
		return nil, fmt.Errorf("run_id must be a DNS-safe Kubernetes name of at most 63 characters")
	}
	command := []string{"generate", "--target", target.URL, "--config", string(input.Scenario)}
	if target.Model != "" {
		command = append(command, "--model", target.Model)
	}
	mounts := []MountSpec{}
	if scenarioUsesDataset(typed.Workload) {
		if s.options.DatasetPVC == "" || s.options.DatasetRoot == "" {
			return nil, fmt.Errorf("the scenario uses a dataset but dataset storage is not configured")
		}
		mounts = append(mounts, MountSpec{PVC: s.options.DatasetPVC, MountPath: s.options.DatasetRoot})
	}
	create := CreateRunRequest{Name: runID, Command: command, Workers: workers, Arch: input.Architecture, CPU: input.CPU, Memory: input.Memory, ActiveDeadlineSeconds: input.DeadlineSecond, Mounts: mounts, Results: &ResultSpec{PVC: s.options.ResultPVC, MountPath: s.options.ResultRoot, Subdir: filepath.ToSlash(filepath.Join("mcp", input.ResultLabel))}}
	name, cfg, effectiveCommand, results, err := s.prepare(create)
	if err != nil {
		return nil, err
	}
	cfg.Platform = platform
	service, job, err := kube.RenderCoreResources(cfg, commandDefaultName(effectiveCommand), effectiveCommand)
	if err != nil {
		return nil, err
	}
	deadline, retention, err := s.runtimeLimits(create)
	if err != nil {
		return nil, err
	}
	job.Spec.ActiveDeadlineSeconds = &deadline
	job.Spec.TTLSecondsAfterFinished = &retention
	effective := effectiveScenario(scenario)
	effectiveJSON, _ := json.Marshal(effective)
	fingerprintJSON, _ := json.Marshal(struct {
		Command  []string
		Config   kube.KubeConfig
		Deadline int64
		TTL      int32
		Target   string
		Scenario json.RawMessage
	}{effectiveCommand, cfg, deadline, retention, input.Target, effectiveJSON})
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(fingerprintJSON))
	warnings := scenarioWarnings(scenario, workers, time.Duration(deadline)*time.Second)
	resources := resourcesFromJob(job, platform, cfg.Arch, workers, s.options.RunnerImage)
	return &plannedBenchmark{RunID: name, Target: plannedTarget{Name: input.Target, Model: target.Model}, EffectiveScenario: effective, Load: plannedLoad(scenario, workers), Resources: resources, Results: results, DeadlineSeconds: deadline, Warnings: warnings, createRequest: create, config: cfg, command: effectiveCommand, service: service, job: job, fingerprint: fingerprint, resultLabel: input.ResultLabel, workstream: input.VDPWorkstream}, nil
}

func (s *Server) submitBenchmark(ctx context.Context, plan *plannedBenchmark) (Run, bool, error) {
	created := s.now().UTC()
	commandJSON, _ := json.Marshal(plan.command)
	resultsJSON, _ := json.Marshal(plan.Results)
	scenarioJSON, _ := json.Marshal(plan.EffectiveScenario)
	metadata := map[string]string{createdAnnotation: created.Format(time.RFC3339Nano), commandAnnotation: string(commandJSON), resultsAnnotation: string(resultsJSON), requestAnnotation: plan.fingerprint, scenarioAnnotation: string(scenarioJSON), targetAnnotation: plan.Target.Name, resultLabelAnnotation: plan.resultLabel, platformAnnotation: plan.Resources.Platform, mcpManagedAnnotation: "true"}
	if plan.workstream != "" {
		metadata[workstreamAnnotation] = plan.workstream
	}
	decorate(&plan.service.ObjectMeta, plan.RunID, metadata)
	decorate(&plan.job.ObjectMeta, plan.RunID, metadata)
	decorate(&plan.job.Spec.Template.ObjectMeta, plan.RunID, nil)
	existing, err := s.client.BatchV1().Jobs(s.namespace).Get(ctx, plan.RunID, metav1.GetOptions{})
	if err == nil {
		if existing.Labels[managedLabel] != "true" || existing.Annotations[requestAnnotation] != plan.fingerprint {
			return Run{}, false, fmt.Errorf("run_id already exists with a different validated specification")
		}
		if err := s.upsertManagedService(ctx, plan.service, plan.fingerprint); err != nil {
			return Run{}, false, friendlyKubeError(err)
		}
		return runFromJob(existing), false, nil
	}
	if !apierrors.IsNotFound(err) {
		return Run{}, false, friendlyKubeError(err)
	}
	if err := s.upsertManagedService(ctx, plan.service, plan.fingerprint); err != nil {
		return Run{}, false, friendlyKubeError(err)
	}
	job, err := s.client.BatchV1().Jobs(s.namespace).Create(ctx, plan.job, metav1.CreateOptions{})
	if err != nil {
		_ = s.deleteServiceIfFingerprint(ctx, plan.RunID, plan.fingerprint)
		return Run{}, false, friendlyKubeError(err)
	}
	return runFromJob(job), true, nil
}

func (s *Server) listBenchmarksMCP(ctx context.Context, input benchmarkListInput) (any, error) {
	if input.Limit == 0 {
		input.Limit = 50
	}
	if input.Limit < 1 || input.Limit > 100 {
		return nil, fmt.Errorf("limit must be between 1 and 100")
	}
	if input.Status != "" && !containsString([]string{"pending", "running", "succeeded", "failed"}, input.Status) {
		return nil, fmt.Errorf("status filter is invalid")
	}
	jobs, err := s.client.BatchV1().Jobs(s.namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.Set{managedLabel: "true"}.String()})
	if err != nil {
		return nil, friendlyKubeError(err)
	}
	runs := make([]Run, 0, len(jobs.Items))
	for i := range jobs.Items {
		run := mcpRunFromJob(&jobs.Items[i])
		if input.Status != "" && run.Status != input.Status {
			continue
		}
		if input.Label != "" && jobs.Items[i].Annotations[resultLabelAnnotation] != input.Label {
			continue
		}
		runs = append(runs, run)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].CreatedAt.After(runs[j].CreatedAt) })
	total := len(runs)
	if len(runs) > input.Limit {
		runs = runs[:input.Limit]
	}
	return map[string]any{"runs": runs, "returned": len(runs), "total": total, "truncated": len(runs) < total}, nil
}

func (s *Server) cancelBenchmarkMCP(ctx context.Context, id string) (any, error) {
	job, err := s.getManagedJob(ctx, id)
	if apierrors.IsNotFound(err) {
		return map[string]any{"run_id": id, "canceled": true, "already_absent": true}, nil
	}
	if err != nil {
		return nil, friendlyKubeError(err)
	}
	if status := jobStatus(job); status == "succeeded" || status == "failed" {
		return nil, fmt.Errorf("terminal benchmark runs cannot be canceled")
	}
	if err := s.client.CoreV1().Services(s.namespace).Delete(ctx, id, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return nil, friendlyKubeError(err)
	}
	propagation := metav1.DeletePropagationBackground
	if err := s.client.BatchV1().Jobs(s.namespace).Delete(ctx, id, metav1.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
		return nil, friendlyKubeError(err)
	}
	return map[string]any{"run_id": id, "canceled": true, "already_absent": false}, nil
}

func (s *Server) listArtifacts(run Run) ([]artifactMetadata, error) {
	dir, err := s.safeResultDirectory(run.Results)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []artifactMetadata{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading benchmark artifacts: %w", err)
	}
	if len(entries) > 512 {
		return nil, fmt.Errorf("artifact directory exceeds the bounded entry limit")
	}
	artifacts := make([]artifactMetadata, 0, len(entries))
	for _, entry := range entries {
		matches := artifactPattern.FindStringSubmatch(entry.Name())
		if entry.IsDir() || matches == nil {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		file, err := os.Open(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("opening artifact metadata: %w", err)
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return nil, fmt.Errorf("hashing artifact %s", entry.Name())
		}
		kind := "prometheus"
		workerText := ""
		if matches[2] != "" {
			kind, workerText = "requests_jsonl", matches[2]
		} else if matches[3] != "" {
			kind, workerText = "timestamps", matches[3]
		}
		var worker *int
		if workerText != "" {
			value, _ := strconv.Atoi(workerText)
			worker = &value
		}
		artifacts = append(artifacts, artifactMetadata{Name: entry.Name(), Kind: kind, Worker: worker, SizeBytes: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)), ModifiedAt: info.ModTime().UTC()})
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Name < artifacts[j].Name })
	return artifacts, nil
}

func (s *Server) getBenchmarkReport(ctx context.Context, id string) (*benchmarkReport, error) {
	job, err := s.getManagedJob(ctx, id)
	if err != nil {
		return nil, friendlyKubeError(err)
	}
	run := runFromJob(job)
	artifacts, err := s.listArtifacts(run)
	if err != nil {
		return nil, err
	}
	image := ""
	if len(job.Spec.Template.Spec.Containers) > 0 {
		image = job.Spec.Template.Spec.Containers[0].Image
	}
	reportRun := run
	reportRun.Command = nil
	report := &benchmarkReport{SchemaVersion: "nyann-bench-report-v1", Run: reportRun, Target: job.Annotations[targetAnnotation], ResultLabel: job.Annotations[resultLabelAnnotation], VDPWorkstream: job.Annotations[workstreamAnnotation], Image: image, Artifacts: artifacts, Warnings: []string{}}
	if value := job.Annotations[scenarioAnnotation]; json.Valid([]byte(value)) {
		report.EffectiveScenario = json.RawMessage(value)
	}
	requestWorkers := map[int]bool{}
	timestampWorkers := map[int]bool{}
	for _, artifact := range artifacts {
		if artifact.Worker == nil {
			continue
		}
		if artifact.Kind == "requests_jsonl" {
			requestWorkers[*artifact.Worker] = true
		} else if artifact.Kind == "timestamps" {
			timestampWorkers[*artifact.Worker] = true
		}
	}
	for worker := 0; worker < int(run.Workers); worker++ {
		if requestWorkers[worker] && timestampWorkers[worker] {
			report.CompleteWorkers = append(report.CompleteWorkers, worker)
		} else {
			report.MissingWorkers = append(report.MissingWorkers, worker)
		}
	}
	report.WorkerComplete = len(report.MissingWorkers) == 0
	dir, err := s.safeResultDirectory(run.Results)
	if err != nil {
		return nil, err
	}
	if len(requestWorkers) > 0 {
		records, loadErr := analysis.LoadRecords(dir)
		start, end, timeErr := analysis.LoadTimestamps(dir)
		if loadErr == nil {
			if timeErr == nil {
				report.MeasurementWindow = &measurementWindow{StartUnixSeconds: start, EndUnixSeconds: end}
				report.Summary = analysis.Compute(records, start, end)
				if duration := end - start; duration > 0 {
					report.Summary.TotalDurationS = duration
					report.Summary.RequestsPerSec = float64(report.Summary.SuccessRequests) / duration
					report.Summary.OutputTokensPerS = float64(report.Summary.TotalOutputTokens) / duration
				}
			} else {
				report.Summary = analysis.Compute(records, 0, 0)
				report.Warnings = append(report.Warnings, "exact measurement window is unavailable: "+timeErr.Error())
			}
		} else {
			report.Warnings = append(report.Warnings, "request artifacts are incomplete: "+loadErr.Error())
		}
	} else {
		report.Warnings = append(report.Warnings, "no request artifacts are available yet")
	}
	if !report.WorkerComplete {
		report.Warnings = append(report.Warnings, "one or more Indexed Job partitions are incomplete")
	}
	return report, nil
}

func (s *Server) safeResultDirectory(results ResultMetadata) (string, error) {
	if !results.Durable || results.Path == "" || s.options.ResultRoot == "" {
		return "", fmt.Errorf("durable result artifacts are unavailable")
	}
	root, err := filepath.Abs(s.options.ResultRoot)
	if err != nil {
		return "", err
	}
	dir, err := filepath.Abs(results.Path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", fmt.Errorf("result path escapes the configured result root")
	}
	if _, statErr := os.Stat(dir); statErr == nil {
		realRoot, rootErr := filepath.EvalSymlinks(root)
		realDir, dirErr := filepath.EvalSymlinks(dir)
		if rootErr != nil || dirErr != nil {
			return "", fmt.Errorf("resolving durable result path")
		}
		realRel, relErr := filepath.Rel(realRoot, realDir)
		if relErr != nil || realRel == "." || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("result path resolves outside the configured result root")
		}
	}
	return dir, nil
}

func mcpRunFromJob(job *batchv1.Job) Run {
	run := runFromJob(job)
	// The native REST API retains the exact CLI command for compatibility. MCP
	// exposes only the logical target and typed effective scenario.
	run.Command = nil
	return run
}

func benchmarkDetails(job *batchv1.Job) map[string]any {
	details := map[string]any{
		"run":          mcpRunFromJob(job),
		"target":       job.Annotations[targetAnnotation],
		"result_label": job.Annotations[resultLabelAnnotation],
		"platform":     job.Annotations[platformAnnotation],
	}
	if value := job.Annotations[scenarioAnnotation]; json.Valid([]byte(value)) {
		details["effective_scenario"] = json.RawMessage(value)
	}
	if value := job.Annotations[workstreamAnnotation]; value != "" {
		details["vdp_workstream"] = value
	}
	if len(job.Spec.Template.Spec.Containers) > 0 {
		details["image"] = job.Spec.Template.Spec.Containers[0].Image
	}
	return details
}

func decodeBenchmarkRef(raw json.RawMessage) (benchmarkRef, error) {
	var ref benchmarkRef
	if err := decodeStrict(raw, &ref); err != nil {
		return ref, err
	}
	if len(validation.IsDNS1123Subdomain(ref.RunID)) > 0 || len(ref.RunID) > 63 {
		return ref, fmt.Errorf("run_id is invalid")
	}
	return ref, nil
}

func validateScenarioBounds(sc *config.ScenarioConfig) error {
	if len(sc.Stages) == 0 || len(sc.Stages) > 128 {
		return fmt.Errorf("scenario must contain between 1 and 128 stages")
	}
	var total time.Duration
	switch sc.Workload.Type {
	case "synthetic", "faker":
	case "corpus":
		if sc.Workload.CorpusPath == "" {
			return fmt.Errorf("corpus workload requires corpus_path")
		}
	case "gsm8k":
		if sc.Workload.GSM8KPath == "" {
			return fmt.Errorf("gsm8k workload requires gsm8k_path")
		}
	case "gpqa":
		if sc.Workload.GPQAPath == "" {
			return fmt.Errorf("gpqa workload requires gpqa_path")
		}
	default:
		return fmt.Errorf("unsupported workload type %q", sc.Workload.Type)
	}
	if sc.Workload.ISL < 0 || sc.Workload.ISL > 1048576 || sc.Workload.OSL < 0 || sc.Workload.OSL > 1048576 || sc.Workload.Turns < 1 || sc.Workload.Turns > 1024 {
		return fmt.Errorf("workload token or turn settings exceed service bounds")
	}
	if sc.Workload.CacheSalt != nil && sc.Workload.CacheSalt.Mode != "random" && sc.Workload.CacheSalt.Mode != "fixed" {
		return fmt.Errorf("cache_salt.mode must be random or fixed")
	}
	if sc.Workload.CacheSalt != nil && sc.Workload.CacheSalt.Mode == "fixed" && sc.Workload.CacheSalt.Value == "" {
		return fmt.Errorf("fixed cache_salt requires a value")
	}
	for i, stage := range sc.Stages {
		if stage.Barrier {
			continue
		}
		if stage.Duration <= 0 || stage.Duration > 24*time.Hour {
			return fmt.Errorf("stage %d duration must be positive and at most 24h", i)
		}
		total += stage.Duration
		if stage.Concurrency < 0 || stage.Concurrency > 65536 || stage.Rate < 0 || stage.Rate > 1000000 || stage.MaxInFlight < 0 || stage.MaxInFlight > 65536 || stage.MaxRequests < 0 || stage.MaxRequests > 1000000000 {
			return fmt.Errorf("stage %d load exceeds service bounds", i)
		}
		if (stage.Mode == "constant" || stage.Mode == "poisson") && stage.Rate <= 0 {
			return fmt.Errorf("stage %d requires a positive rate", i)
		}
		if stage.Mode != "constant" && stage.Mode != "poisson" && stage.Concurrency <= 0 {
			return fmt.Errorf("stage %d requires positive concurrency", i)
		}
	}
	if total > 7*24*time.Hour {
		return fmt.Errorf("total scenario duration exceeds 7 days")
	}
	return nil
}

func validateDatasetPaths(workload config.Workload, root string) error {
	for field, value := range map[string]string{"corpus_path": workload.CorpusPath, "gsm8k_path": workload.GSM8KPath, "gsm8k_train_path": workload.GSM8KTrainPath, "gpqa_path": workload.GPQAPath} {
		if value == "" {
			continue
		}
		if root == "" || !pathWithinRoot(root, value) {
			return fmt.Errorf("workload.%s must be beneath the operator-configured dataset root", field)
		}
	}
	return nil
}

func pathWithinRoot(root, value string) bool {
	if !filepath.IsAbs(value) || filepath.Clean(value) != value {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), value)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func scenarioUsesDataset(workload config.Workload) bool {
	return workload.CorpusPath != "" || workload.GSM8KPath != "" || workload.GSM8KTrainPath != "" || workload.GPQAPath != ""
}

func effectiveScenario(sc *config.ScenarioConfig) map[string]any {
	stages := make([]map[string]any, 0, len(sc.Stages))
	for _, stage := range sc.Stages {
		if stage.Barrier {
			stages = append(stages, map[string]any{"barrier": true, "drain": stage.BarrierDrain})
			continue
		}
		mode := stage.Mode
		if mode == "" {
			mode = "concurrent"
		}
		stages = append(stages, map[string]any{"name": stage.Name, "mode": mode, "duration_seconds": stage.Duration.Seconds(), "concurrency": stage.Concurrency, "conversation_pool_size": stage.ConversationPoolSize, "rate": stage.Rate, "max_inflight": stage.MaxInFlight, "max_requests": stage.MaxRequests, "warmup": stage.Warmup})
	}
	return map[string]any{"workload": sc.Workload, "stages": stages}
}

func plannedLoad(sc *config.ScenarioConfig, workers int) []plannedStage {
	result := []plannedStage{}
	index := 0
	for _, stage := range sc.Stages {
		if stage.Barrier {
			continue
		}
		mode := stage.Mode
		if mode == "" {
			mode = "concurrent"
		}
		item := plannedStage{Index: index, Name: stage.Name, Mode: mode, DurationSeconds: stage.Duration.Seconds(), TotalConcurrency: stage.Concurrency, TotalRate: stage.Rate}
		for worker := 0; worker < workers; worker++ {
			if mode == "constant" || mode == "poisson" {
				item.PerWorkerRate = append(item.PerWorkerRate, config.DivideRate(stage.Rate, workers))
			} else {
				item.PerWorkerConcurrency = append(item.PerWorkerConcurrency, config.DivideConcurrency(stage.Concurrency, workers, worker))
			}
		}
		result = append(result, item)
		index++
	}
	return result
}

func scenarioWarnings(sc *config.ScenarioConfig, workers int, deadline time.Duration) []string {
	warnings := []string{}
	var duration time.Duration
	for i, stage := range sc.Stages {
		if !stage.Barrier {
			duration += stage.Duration
		}
		if !stage.Barrier && stage.Mode != "constant" && stage.Mode != "poisson" && stage.Concurrency < workers {
			warnings = append(warnings, fmt.Sprintf("stage %d has fewer concurrent streams than workers; some partitions will be idle", i))
		}
	}
	if duration > deadline {
		warnings = append(warnings, fmt.Sprintf("scenario duration %s exceeds the Job deadline %s", duration, deadline))
	}
	return warnings
}

func resourcesFromJob(job *batchv1.Job, platform, arch string, workers int, image string) resourceSummary {
	cpu, memory, totalMemory := "", "", ""
	if len(job.Spec.Template.Spec.Containers) > 0 {
		resources := job.Spec.Template.Spec.Containers[0].Resources
		cpu = resources.Limits.Cpu().String()
		memory = resources.Limits.Memory().String()
		memoryQuantity := resources.Limits.Memory().DeepCopy()
		memoryQuantity.Set(memoryQuantity.Value() * int64(workers))
		totalMemory = memoryQuantity.String()
	}
	totalCPU := ""
	if quantity := job.Spec.Template.Spec.Containers[0].Resources.Limits.Cpu(); quantity != nil {
		copy := quantity.DeepCopy()
		copy.SetMilli(copy.MilliValue() * int64(workers))
		totalCPU = copy.String()
	}
	return resourceSummary{Platform: platform, Architecture: arch, Workers: workers, CPUPerWorker: cpu, MemoryPerWork: memory, TotalCPU: totalCPU, TotalMemory: totalMemory, GPURequested: false, Queue: "none (CPU Indexed Job)", Image: image}
}

func decodeStrict(raw json.RawMessage, value any) error {
	if len(raw) == 0 {
		return fmt.Errorf("missing JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	return ensureEOF(dec)
}

func jsonObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid(trimmed)
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func decodeMCPName(value string) (string, error) {
	if strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(value, "=?base64?"), "?="))
		if err != nil || !utf8.Valid(decoded) {
			return "", fmt.Errorf("invalid encoded MCP name")
		}
		return string(decoded), nil
	}
	return value, nil
}

func friendlyKubeError(err error) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("benchmark not found")
	}
	if apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("benchmark already exists")
	}
	return err
}

func mcpResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeMCPJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": rawID(id), "result": completeMCPResult(result)})
}

func mcpToolResult(w http.ResponseWriter, id json.RawMessage, value any, isError bool) {
	structured, err := json.Marshal(value)
	if err != nil {
		value = map[string]any{"error": "MCP result exceeds the 1 MiB bounded response limit", "maximum_bytes": mcpMaximumResultBytes}
		structured, _ = json.Marshal(value)
		isError = true
	}
	result := map[string]any{"content": []map[string]string{{"type": "text", "text": string(structured)}}, "structuredContent": value, "isError": isError}
	envelope := map[string]any{"jsonrpc": "2.0", "id": rawID(id), "result": completeMCPResult(result)}
	encoded, marshalErr := json.Marshal(envelope)
	if marshalErr != nil || len(encoded) > mcpMaximumResultBytes {
		value = map[string]any{"error": "MCP result exceeds the 1 MiB bounded response limit", "maximum_bytes": mcpMaximumResultBytes}
		structured, _ = json.Marshal(value)
		result = map[string]any{"content": []map[string]string{{"type": "text", "text": string(structured)}}, "structuredContent": value, "isError": true}
		envelope = map[string]any{"jsonrpc": "2.0", "id": rawID(id), "result": completeMCPResult(result)}
	}
	writeMCPJSON(w, http.StatusOK, envelope)
}

func mcpError(w http.ResponseWriter, id json.RawMessage, status, code int, message string, data any) {
	errorValue := map[string]any{"code": code, "message": message}
	if data != nil {
		errorValue["data"] = data
	}
	writeMCPJSON(w, status, map[string]any{"jsonrpc": "2.0", "id": rawID(id), "error": errorValue})
}

func rawID(value json.RawMessage) any {
	if len(value) == 0 {
		return nil
	}
	var result any
	if json.Unmarshal(value, &result) != nil {
		return nil
	}
	return result
}

func validMCPID(value json.RawMessage) bool {
	var decoded any
	if len(value) == 0 || json.Unmarshal(value, &decoded) != nil {
		return false
	}
	switch typed := decoded.(type) {
	case string:
		return true
	case float64:
		return typed == float64(int64(typed))
	default:
		return false
	}
}

func completeMCPResult(value any) map[string]any {
	result := map[string]any{"resultType": "complete", "_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]string{"name": mcpServerName, "version": mcpServerVersion}}}
	if fields, ok := value.(map[string]any); ok {
		for name, field := range fields {
			result[name] = field
		}
	}
	return result
}

func writeMCPJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) mcpTools() []map[string]any {
	readOnly := map[string]bool{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
	submit := map[string]bool{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
	cancel := map[string]bool{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true, "openWorldHint": false}
	benchmarkSchema := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"target", "scenario", "result_label"}, "properties": map[string]any{"run_id": boundedString(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`, 63), "target": map[string]any{"type": "string", "enum": s.targetNames()}, "scenario": map[string]any{"type": "object", "additionalProperties": false, "required": []string{}, "maxProperties": 5, "properties": scenarioProperties()}, "workers": map[string]any{"type": "integer", "minimum": 1, "maximum": s.options.MaxWorkers}, "cpu": boundedString(`^[0-9]+(?:m|(?:\.[0-9]+)?)?$`, 16), "memory": boundedString(`^[0-9]+(?:Ki|Mi|Gi|Ti)?$`, 16), "platform": map[string]any{"type": "string", "enum": s.options.allowedPlatforms()}, "architecture": map[string]any{"type": "string", "enum": []string{"amd64", "arm64"}}, "deadline_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(s.options.MaxActiveDeadline / time.Second)}, "result_label": boundedString(`^[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?$`, 63), "vdp_workstream": boundedString(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`, 128)}}
	refSchema := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"run_id"}, "properties": map[string]any{"run_id": boundedString(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`, 63)}}
	listSchema := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"status": map[string]any{"type": "string", "enum": []string{"pending", "running", "succeeded", "failed"}}, "result_label": boundedString(`^[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?$`, 63), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}}
	tools := []map[string]any{
		toolDefinition("plan_benchmark", "Validate and render a benchmark without cluster mutation.", benchmarkSchema, readOnly),
		toolDefinition("submit_benchmark", "Submit a previously plannable CPU Indexed Job using an operator-owned logical target.", benchmarkSchema, submit),
		toolDefinition("list_benchmarks", "List a bounded set of benchmark runs.", listSchema, readOnly),
		toolDefinition("get_benchmark", "Get one benchmark's status and durable result metadata.", refSchema, readOnly),
		toolDefinition("cancel_benchmark", "Cancel and clean up one non-terminal benchmark.", refSchema, cancel),
		toolDefinition("list_benchmark_artifacts", "List artifact metadata and hashes without returning raw payloads.", refSchema, readOnly),
		toolDefinition("get_benchmark_report", "Aggregate all available worker partitions into a bounded report.", refSchema, readOnly),
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i]["name"].(string) < tools[j]["name"].(string) })
	return tools
}

func isMCPTool(name string) bool {
	switch name {
	case "plan_benchmark", "submit_benchmark", "list_benchmarks", "get_benchmark", "cancel_benchmark", "list_benchmark_artifacts", "get_benchmark_report":
		return true
	default:
		return false
	}
}

func (s *Server) targetNames() []string {
	names := make([]string, 0, len(s.options.InferenceTargets))
	for name := range s.options.InferenceTargets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func toolDefinition(name, description string, schema map[string]any, annotations map[string]bool) map[string]any {
	return map[string]any{"name": name, "description": description, "inputSchema": schema, "annotations": annotations}
}

func boundedString(pattern string, maximum int) map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "maxLength": maximum, "pattern": pattern}
}

func scenarioProperties() map[string]any {
	duration := map[string]any{"oneOf": []any{map[string]any{"type": "string", "minLength": 1, "maxLength": 32}, map[string]any{"type": "number", "exclusiveMinimum": 0, "maximum": 86400}}}
	load := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"mode": map[string]any{"type": "string", "enum": []string{"concurrent", "conversation_pool", "constant", "poisson"}}, "concurrency": map[string]any{"type": "integer", "minimum": 0, "maximum": 65536}, "conversation_pool_size": map[string]any{"type": "integer", "minimum": 0, "maximum": 1000000}, "rate": map[string]any{"type": "number", "minimum": 0, "maximum": 1000000}, "max_inflight": map[string]any{"type": "integer", "minimum": 0, "maximum": 65536}, "rampup": duration, "duration": duration}}
	stage := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"concurrency", "duration"}, "properties": map[string]any{"concurrency": map[string]any{"type": "integer", "minimum": 0, "maximum": 65536}, "conversation_pool_size": map[string]any{"type": "integer", "minimum": 0, "maximum": 1000000}, "duration": duration, "max_requests": map[string]any{"type": "integer", "minimum": 0, "maximum": 1000000000}}}
	sweep := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"min", "max", "steps", "step_duration"}, "properties": map[string]any{"min": map[string]any{"type": "integer", "minimum": 1, "maximum": 65536}, "max": map[string]any{"type": "integer", "minimum": 1, "maximum": 65536}, "steps": map[string]any{"type": "integer", "minimum": 1, "maximum": 128}, "step_duration": duration}}
	workload := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"type": map[string]any{"type": "string", "enum": []string{"synthetic", "faker", "corpus", "gsm8k", "gpqa"}}, "name": boundedString(`^[^\x00]+$`, 128), "isl": map[string]any{"type": "integer", "minimum": 0, "maximum": 1048576}, "subsequent_isl": map[string]any{"type": "integer", "minimum": 0, "maximum": 1048576}, "osl": map[string]any{"type": "integer", "minimum": 0, "maximum": 1048576}, "turns": map[string]any{"type": "integer", "minimum": 1, "maximum": 1024}, "corpus_path": boundedString(`^/[^\x00]*$`, 4096), "gsm8k_path": boundedString(`^/[^\x00]*$`, 4096), "gsm8k_train_path": boundedString(`^/[^\x00]*$`, 4096), "num_fewshot": map[string]any{"type": "integer", "minimum": 0, "maximum": 128}, "gpqa_path": boundedString(`^/[^\x00]*$`, 4096), "chars_per_token": map[string]any{"type": "number", "minimum": 0, "maximum": 100}, "cache_salt": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"mode"}, "properties": map[string]any{"mode": map[string]any{"type": "string", "enum": []string{"random", "fixed"}}, "value": boundedString(`^[^\x00]+$`, 4096)}}}}
	warmup := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"duration"}, "properties": map[string]any{"duration": duration, "stagger": map[string]any{"type": "boolean"}}}
	return map[string]any{"load": load, "stages": map[string]any{"type": "array", "maxItems": 128, "items": stage}, "sweep": sweep, "warmup": warmup, "workload": workload}
}
