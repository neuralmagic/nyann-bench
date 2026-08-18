package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/neuralmagic/nyann-bench/pkg/analysis"
	"github.com/neuralmagic/nyann-bench/pkg/config"
	"github.com/neuralmagic/nyann-bench/pkg/kube"
)

const (
	mcpProtocolVersion      = "2026-07-28"
	mcpMaximumRequestBytes  = 1 << 20
	mcpMaximumResultBytes   = 1 << 20
	mcpMaximumScenarioBytes = config.MaxScenarioInputBytes
	scenarioAnnotation      = "nyann-bench.neuralmagic.com/effective-scenario"
	targetAnnotation        = "nyann-bench.neuralmagic.com/target"
	resultLabelAnnotation   = "nyann-bench.neuralmagic.com/result-label"
	platformAnnotation      = "nyann-bench.neuralmagic.com/platform"
	workstreamAnnotation    = "vdp.neuralmagic.com/workstream"
	mcpManagedAnnotation    = "nyann-bench.neuralmagic.com/mcp-managed"
	mcpServerName           = "nyann-bench"
	mcpServerVersion        = "0.2.0"
)

var (
	resultLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,61}[a-z0-9])?$`)
	attachmentPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	artifactPattern    = regexp.MustCompile(`^(requests_([0-9]+)\.jsonl|timestamps_([0-9]+)\.json|prometheus(?:_[A-Za-z0-9.-]+)?\.json|run-manifest\.json)$`)
)

type benchmarkInput struct {
	RunID          string          `json:"run_id,omitempty"`
	Target         string          `json:"target"`
	Scenario       json.RawMessage `json:"scenario,omitempty"`
	Starlark       string          `json:"starlark,omitempty"`
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
	RetentionSeconds  int32           `json:"retention_seconds"`
	Warnings          []string        `json:"validation_warnings"`
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

type durableRunManifest struct {
	SchemaVersion     string          `json:"schema_version"`
	RunID             string          `json:"run_id"`
	Fingerprint       string          `json:"fingerprint"`
	CreatedAt         time.Time       `json:"created_at"`
	Target            plannedTarget   `json:"target"`
	ResultLabel       string          `json:"result_label"`
	VDPWorkstream     string          `json:"vdp_workstream,omitempty"`
	EffectiveScenario json.RawMessage `json:"effective_scenario"`
	Resources         resourceSummary `json:"resources"`
	Results           ResultMetadata  `json:"results"`
	Workers           int32           `json:"workers"`
	DeadlineSeconds   int64           `json:"deadline_seconds"`
	RetentionSeconds  int32           `json:"retention_seconds"`
	Image             string          `json:"image"`
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
		plan, err := s.planBenchmark(ctx, input)
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
		if apierrors.IsNotFound(err) {
			manifest, manifestErr := s.loadRunManifest(ref.RunID)
			if manifestErr != nil {
				if !errors.Is(manifestErr, os.ErrNotExist) {
					return nil, manifestErr
				}
				return nil, friendlyKubeError(err)
			}
			return benchmarkDetailsFromManifest(manifest), nil
		}
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
		var run Run
		if apierrors.IsNotFound(err) {
			manifest, manifestErr := s.loadRunManifest(ref.RunID)
			if manifestErr != nil {
				if !errors.Is(manifestErr, os.ErrNotExist) {
					return nil, manifestErr
				}
				return nil, friendlyKubeError(err)
			}
			run = runFromManifest(manifest, "archived")
		} else if err != nil {
			return nil, friendlyKubeError(err)
		} else {
			run = runFromJob(job)
		}
		artifacts, err := s.listArtifacts(ctx, run)
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
