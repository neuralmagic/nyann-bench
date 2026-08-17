package controlapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/neuralmagic/nyann-bench/pkg/config"
)

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
	encoded, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		encoded = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"Internal JSON encoding error"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(encoded, '\n'))
}

func (s *Server) benchmarkInputSchema() map[string]any {
	properties := map[string]any{
		"run_id":           boundedString(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`, 63),
		"target":           map[string]any{"type": "string", "enum": s.targetNames()},
		"scenario":         map[string]any{"type": "object", "additionalProperties": false, "maxProperties": 5, "properties": scenarioProperties()},
		"starlark":         map[string]any{"type": "string", "minLength": 1, "maxLength": mcpMaximumScenarioBytes, "description": "Inline nyann-bench Starlark source. load() is disabled and operator-owned target/model settings cannot be overridden."},
		"workers":          map[string]any{"type": "integer", "minimum": 1, "maximum": s.options.MaxWorkers},
		"cpu":              boundedString(`^[0-9]+(?:m|(?:\.[0-9]+)?)?$`, 16),
		"memory":           boundedString(`^[0-9]+(?:Ki|Mi|Gi|Ti)?$`, 16),
		"platform":         map[string]any{"type": "string", "enum": s.options.allowedPlatforms()},
		"architecture":     map[string]any{"type": "string", "enum": []string{"amd64", "arm64"}},
		"deadline_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": int64(s.options.MaxActiveDeadline / time.Second)},
		"result_label":     boundedString(`^[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?$`, 63),
		"vdp_workstream":   boundedString(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`, 128),
	}
	chooseScenario := []any{
		map[string]any{"required": []string{"scenario"}, "not": map[string]any{"required": []string{"starlark"}}},
		map[string]any{"required": []string{"starlark"}, "not": map[string]any{"required": []string{"scenario"}}},
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"target", "result_label"},
		"oneOf":                chooseScenario,
		"properties":           properties,
	}
}

func (s *Server) mcpTools() []map[string]any {
	readOnly := map[string]bool{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
	submit := map[string]bool{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
	cancel := map[string]bool{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true, "openWorldHint": false}
	benchmarkSchema := s.benchmarkInputSchema()
	refSchema := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"run_id"}, "properties": map[string]any{"run_id": boundedString(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`, 63)}}
	listSchema := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"status": map[string]any{"type": "string", "enum": []string{"pending", "running", "succeeded", "failed", "archived"}}, "result_label": boundedString(`^[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?$`, 63), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}}
	tools := []map[string]any{
		toolDefinition("plan_benchmark", "Validate and render a JSON or programmable Starlark benchmark without cluster mutation.", benchmarkSchema, readOnly),
		toolDefinition("submit_benchmark", "Submit a previously plannable JSON or programmable Starlark CPU Indexed Job using an operator-owned logical target.", benchmarkSchema, submit),
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
	sweep := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"min", "max", "steps", "step_duration"}, "properties": map[string]any{"min": map[string]any{"type": "integer", "minimum": 1, "maximum": 65536}, "max": map[string]any{"type": "integer", "minimum": 1, "maximum": 65536}, "steps": map[string]any{"type": "integer", "minimum": 1, "maximum": config.MaxScenarioStages}, "step_duration": duration}}
	workload := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"type": map[string]any{"type": "string", "enum": []string{"synthetic", "faker", "corpus", "gsm8k", "gpqa"}}, "name": boundedString(`^[^\x00]+$`, 128), "isl": map[string]any{"type": "integer", "minimum": 0, "maximum": 1048576}, "subsequent_isl": map[string]any{"type": "integer", "minimum": 0, "maximum": 1048576}, "osl": map[string]any{"type": "integer", "minimum": 0, "maximum": 1048576}, "turns": map[string]any{"type": "integer", "minimum": 1, "maximum": 1024}, "corpus_path": boundedString(`^/[^\x00]*$`, 4096), "gsm8k_path": boundedString(`^/[^\x00]*$`, 4096), "gsm8k_train_path": boundedString(`^/[^\x00]*$`, 4096), "num_fewshot": map[string]any{"type": "integer", "minimum": 0, "maximum": 128}, "gpqa_path": boundedString(`^/[^\x00]*$`, 4096), "chars_per_token": map[string]any{"type": "number", "minimum": 0, "maximum": 100}, "cache_salt": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"mode"}, "properties": map[string]any{"mode": map[string]any{"type": "string", "enum": []string{"random", "fixed"}}, "value": boundedString(`^[^\x00]+$`, 4096)}}}}
	warmup := map[string]any{"type": "object", "additionalProperties": false, "required": []string{"duration"}, "properties": map[string]any{"duration": duration, "stagger": map[string]any{"type": "boolean"}}}
	return map[string]any{"load": load, "stages": map[string]any{"type": "array", "maxItems": config.MaxScenarioStages, "items": stage}, "sweep": sweep, "warmup": warmup, "workload": workload}
}
