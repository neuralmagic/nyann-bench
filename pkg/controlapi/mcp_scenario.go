package controlapi

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/neuralmagic/nyann-bench/pkg/config"
)

func (s *Server) parseBenchmarkScenario(ctx context.Context, input benchmarkInput) (*config.ScenarioConfig, error) {
	hasJSON := len(input.Scenario) > 0
	hasStarlark := input.Starlark != ""
	if hasJSON == hasStarlark {
		return nil, fmt.Errorf("exactly one of scenario or starlark must be supplied")
	}
	if hasStarlark {
		if len(input.Starlark) > mcpMaximumScenarioBytes || strings.ContainsRune(input.Starlark, '\x00') {
			return nil, fmt.Errorf("starlark must be non-empty source no larger than %d bytes", mcpMaximumScenarioBytes)
		}
		scenario, err := s.compileStarlark(ctx, input.Starlark)
		if err != nil {
			return nil, fmt.Errorf("invalid Starlark scenario: %w", err)
		}
		return scenario, nil
	}
	if len(input.Scenario) > mcpMaximumScenarioBytes || !jsonObject(input.Scenario) {
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
	return scenario, nil
}

func validateScenarioBounds(sc *config.ScenarioConfig) error {
	if len(sc.Stages) == 0 || len(sc.Stages) > config.MaxScenarioStages {
		return fmt.Errorf("scenario must contain between 1 and %d stages", config.MaxScenarioStages)
	}
	if sc.Target != "" || sc.Model != "" {
		return fmt.Errorf("scenario target and model overrides are not allowed; use the operator-configured logical target")
	}
	var total time.Duration
	if err := validateWorkloadBounds(sc.Workload, "workload"); err != nil {
		return err
	}
	workStages := 0
	for i, stage := range sc.Stages {
		if len(stage.Name) > 128 || strings.ContainsRune(stage.Name, '\x00') {
			return fmt.Errorf("stage %d name exceeds service bounds", i)
		}
		if stage.Target != "" || stage.Model != "" {
			return fmt.Errorf("stage %d target and model overrides are not allowed; use the operator-configured logical target", i)
		}
		if stage.Workload != nil {
			if err := validateWorkloadBounds(*stage.Workload, fmt.Sprintf("stage %d workload", i)); err != nil {
				return err
			}
		}
		if stage.Barrier {
			continue
		}
		workStages++
		if stage.Duration <= 0 || stage.Duration > 24*time.Hour {
			return fmt.Errorf("stage %d duration must be positive and at most 24h", i)
		}
		total += stage.Duration
		if stage.Rampup < 0 || stage.Rampup > 24*time.Hour {
			return fmt.Errorf("stage %d rampup exceeds service bounds", i)
		}
		if stage.Concurrency < 0 || stage.Concurrency > 65536 || stage.ConversationPoolSize < 0 || stage.ConversationPoolSize > 1000000 || math.IsNaN(stage.Rate) || math.IsInf(stage.Rate, 0) || stage.Rate < 0 || stage.Rate > 1000000 || stage.MaxInFlight < 0 || stage.MaxInFlight > 65536 || stage.MaxRequests < 0 || stage.MaxRequests > 1000000000 {
			return fmt.Errorf("stage %d load exceeds service bounds", i)
		}
		if (stage.Mode == "constant" || stage.Mode == "poisson") && stage.Rate <= 0 {
			return fmt.Errorf("stage %d requires a positive rate", i)
		}
		if stage.Mode != "constant" && stage.Mode != "poisson" && stage.Concurrency <= 0 {
			return fmt.Errorf("stage %d requires positive concurrency", i)
		}
	}
	if workStages == 0 {
		return fmt.Errorf("scenario must contain at least one benchmark stage")
	}
	if total > 7*24*time.Hour {
		return fmt.Errorf("total scenario duration exceeds 7 days")
	}
	return nil
}

func validateWorkloadBounds(workload config.Workload, location string) error {
	switch workload.Type {
	case "synthetic", "faker":
	case "corpus":
		if workload.CorpusPath == "" {
			return fmt.Errorf("%s corpus workload requires corpus_path", location)
		}
	case "gsm8k":
		if workload.GSM8KPath == "" {
			return fmt.Errorf("%s gsm8k workload requires gsm8k_path", location)
		}
	case "gpqa":
		if workload.GPQAPath == "" {
			return fmt.Errorf("%s gpqa workload requires gpqa_path", location)
		}
	default:
		return fmt.Errorf("%s has unsupported workload type %q", location, workload.Type)
	}
	if workload.ISL < 0 || workload.ISL > 1048576 || workload.OSL < 0 || workload.OSL > 1048576 || workload.Turns < 1 || workload.Turns > 1024 {
		return fmt.Errorf("%s token or turn settings exceed service bounds", location)
	}
	if workload.SubsequentISL != nil && (*workload.SubsequentISL < 0 || *workload.SubsequentISL > 1048576) {
		return fmt.Errorf("%s subsequent_isl exceeds service bounds", location)
	}
	if workload.NumFewShot != nil && (*workload.NumFewShot < 0 || *workload.NumFewShot > 128) {
		return fmt.Errorf("%s num_fewshot exceeds service bounds", location)
	}
	if math.IsNaN(workload.CharsPerToken) || math.IsInf(workload.CharsPerToken, 0) || workload.CharsPerToken < 0 || workload.CharsPerToken > 100 {
		return fmt.Errorf("%s chars_per_token exceeds service bounds", location)
	}
	if len(workload.Name) > 128 || len(workload.CorpusPath) > 4096 || len(workload.GSM8KPath) > 4096 || len(workload.GSM8KTrainPath) > 4096 || len(workload.GPQAPath) > 4096 || strings.ContainsRune(workload.Name, '\x00') {
		return fmt.Errorf("%s string field exceeds service bounds", location)
	}
	if workload.CacheSalt != nil && workload.CacheSalt.Mode != "random" && workload.CacheSalt.Mode != "fixed" {
		return fmt.Errorf("cache_salt.mode must be random or fixed")
	}
	if workload.CacheSalt != nil && workload.CacheSalt.Mode == "fixed" && workload.CacheSalt.Value == "" {
		return fmt.Errorf("fixed cache_salt requires a value")
	}
	if workload.CacheSalt != nil && (len(workload.CacheSalt.Value) > 4096 || strings.ContainsRune(workload.CacheSalt.Value, '\x00')) {
		return fmt.Errorf("cache_salt.value exceeds service bounds")
	}
	return nil
}

func validateDatasetPaths(workload config.Workload, root, location string) error {
	for field, value := range map[string]string{"corpus_path": workload.CorpusPath, "gsm8k_path": workload.GSM8KPath, "gsm8k_train_path": workload.GSM8KTrainPath, "gpqa_path": workload.GPQAPath} {
		if value == "" {
			continue
		}
		if root == "" || !pathWithinRoot(root, value) {
			return fmt.Errorf("%s.%s must be beneath the operator-configured dataset root", location, field)
		}
	}
	return nil
}

func validateScenarioDatasetPaths(sc *config.ScenarioConfig, root string) error {
	if err := validateDatasetPaths(sc.Workload, root, "workload"); err != nil {
		return err
	}
	for i, stage := range sc.Stages {
		if stage.Workload != nil {
			if err := validateDatasetPaths(*stage.Workload, root, fmt.Sprintf("stage %d workload", i)); err != nil {
				return err
			}
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

func workloadUsesDataset(workload config.Workload) bool {
	return workload.CorpusPath != "" || workload.GSM8KPath != "" || workload.GSM8KTrainPath != "" || workload.GPQAPath != ""
}

func scenarioUsesDataset(sc *config.ScenarioConfig) bool {
	if workloadUsesDataset(sc.Workload) {
		return true
	}
	for _, stage := range sc.Stages {
		if stage.Workload != nil && workloadUsesDataset(*stage.Workload) {
			return true
		}
	}
	return false
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
		effective := map[string]any{"name": stage.Name, "mode": mode, "duration_seconds": stage.Duration.Seconds(), "concurrency": stage.Concurrency, "conversation_pool_size": stage.ConversationPoolSize, "rate": stage.Rate, "max_inflight": stage.MaxInFlight, "max_requests": stage.MaxRequests, "warmup": stage.Warmup}
		if stage.Rampup > 0 {
			effective["rampup_seconds"] = stage.Rampup.Seconds()
		}
		if stage.Workload != nil {
			effective["workload"] = *stage.Workload
		}
		stages = append(stages, effective)
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
