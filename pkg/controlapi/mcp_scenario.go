package controlapi

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"

	"github.com/neuralmagic/nyann-bench/pkg/config"
)

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
	if sc.Workload.SubsequentISL != nil && (*sc.Workload.SubsequentISL < 0 || *sc.Workload.SubsequentISL > 1048576) {
		return fmt.Errorf("workload subsequent_isl exceeds service bounds")
	}
	if sc.Workload.NumFewShot != nil && (*sc.Workload.NumFewShot < 0 || *sc.Workload.NumFewShot > 128) {
		return fmt.Errorf("workload num_fewshot exceeds service bounds")
	}
	if math.IsNaN(sc.Workload.CharsPerToken) || math.IsInf(sc.Workload.CharsPerToken, 0) || sc.Workload.CharsPerToken < 0 || sc.Workload.CharsPerToken > 100 {
		return fmt.Errorf("workload chars_per_token exceeds service bounds")
	}
	if len(sc.Workload.Name) > 128 || len(sc.Workload.CorpusPath) > 4096 || len(sc.Workload.GSM8KPath) > 4096 || len(sc.Workload.GSM8KTrainPath) > 4096 || len(sc.Workload.GPQAPath) > 4096 {
		return fmt.Errorf("workload string field exceeds service bounds")
	}
	if sc.Workload.CacheSalt != nil && sc.Workload.CacheSalt.Mode != "random" && sc.Workload.CacheSalt.Mode != "fixed" {
		return fmt.Errorf("cache_salt.mode must be random or fixed")
	}
	if sc.Workload.CacheSalt != nil && sc.Workload.CacheSalt.Mode == "fixed" && sc.Workload.CacheSalt.Value == "" {
		return fmt.Errorf("fixed cache_salt requires a value")
	}
	if sc.Workload.CacheSalt != nil && len(sc.Workload.CacheSalt.Value) > 4096 {
		return fmt.Errorf("cache_salt.value exceeds service bounds")
	}
	for i, stage := range sc.Stages {
		if stage.Barrier {
			continue
		}
		if stage.Duration <= 0 || stage.Duration > 24*time.Hour {
			return fmt.Errorf("stage %d duration must be positive and at most 24h", i)
		}
		total += stage.Duration
		if stage.Concurrency < 0 || stage.Concurrency > 65536 || stage.ConversationPoolSize < 0 || stage.ConversationPoolSize > 1000000 || stage.Rate < 0 || stage.Rate > 1000000 || stage.MaxInFlight < 0 || stage.MaxInFlight > 65536 || stage.MaxRequests < 0 || stage.MaxRequests > 1000000000 {
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
