package analysis

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/config"
	"github.com/neuralmagic/nyann-bench/pkg/prometheus"
	"github.com/neuralmagic/nyann-bench/pkg/recorder"
)

// CongestionRoleMetrics is the stage observation for one vLLM role.
type CongestionRoleMetrics struct {
	Role        string
	WaitingP50  float64
	TTFTP99     float64
	KVUsageMax  float64
	Preemptions float64
}

// CongestionResult explains whether and why a stage sweep stopped.
type CongestionResult struct {
	Congested bool
	Reasons   []string
	Roles     []CongestionRoleMetrics
}

// CheckStageCongestion checks aggregate vLLM or both halves of a disaggregated
// prefill/decode deployment. Queue depth and TTFT may span roles because they
// form one end-to-end latency signal. KV usage and preemptions must occur on
// the same role, so unrelated prefill/decode observations cannot combine into
// a false cache-congestion result.
func CheckStageCongestion(client *prometheus.Client, ts recorder.StageTimestamp, deployName string, condition config.CongestionCondition) (CongestionResult, error) {
	type roleTarget struct {
		role string
		job  string
		pods string
	}
	roles := []roleTarget{{role: "aggregate", job: "vllm-aggregate", pods: deployName + ".*"}}
	if strings.Contains(deployName, "-decode") {
		roles = []roleTarget{
			{role: "prefill", job: "vllm-prefill", pods: strings.Replace(deployName, "-decode", "-prefill", 1) + ".*"},
			{role: "decode", job: "vllm-decode", pods: deployName + ".*"},
		}
	}

	start := floatToTime(ts.StartTime)
	end := floatToTime(ts.EndTime)
	steadyStart, steadyEnd := trimStage(start, end)
	if steadyEnd.Sub(steadyStart) < 5*time.Second {
		steadyStart, steadyEnd = start, end
	}
	windowSeconds := int(end.Sub(start).Seconds())
	if windowSeconds < 1 {
		windowSeconds = 1
	}

	result := CongestionResult{Roles: make([]CongestionRoleMetrics, len(roles))}
	var wg sync.WaitGroup
	errCh := make(chan error, len(roles)*4)
	for i, target := range roles {
		result.Roles[i].Role = target.role
		labels := fmt.Sprintf(`job="%s", pod=~"%s"`, target.job, target.pods)
		wg.Add(4)
		go func(dst *float64) {
			defer wg.Done()
			stats, err := client.QueryGaugeStats(fmt.Sprintf(`sum(vllm:num_requests_waiting{%s})`, labels), steadyStart, steadyEnd)
			if err != nil {
				errCh <- fmt.Errorf("%s waiting requests: %w", target.role, err)
				return
			}
			*dst = stats.P50
		}(&result.Roles[i].WaitingP50)
		go func(dst *float64) {
			defer wg.Done()
			stats, err := client.HistogramQuantile("vllm:time_to_first_token_seconds_bucket", target.pods, target.job, steadyStart, steadyEnd)
			if err != nil {
				errCh <- fmt.Errorf("%s TTFT: %w", target.role, err)
				return
			}
			*dst = stats.P99
		}(&result.Roles[i].TTFTP99)
		go func(dst *float64) {
			defer wg.Done()
			stats, err := client.QueryGaugeStats(fmt.Sprintf(`max(vllm:kv_cache_usage_perc{%s})`, labels), steadyStart, steadyEnd)
			if err != nil {
				errCh <- fmt.Errorf("%s KV cache: %w", target.role, err)
				return
			}
			*dst = stats.Max
		}(&result.Roles[i].KVUsageMax)
		go func(dst *float64) {
			defer wg.Done()
			q := fmt.Sprintf(`sum(increase(vllm:num_preemptions_total{%s}[%ds]))`, labels, windowSeconds)
			points, err := client.QueryRange(q, end, end, time.Second)
			if err != nil {
				errCh <- fmt.Errorf("%s preemptions: %w", target.role, err)
				return
			}
			if len(points) > 0 {
				*dst = points[len(points)-1].Value
			}
		}(&result.Roles[i].Preemptions)
	}
	wg.Wait()
	close(errCh)

	var maxWaiting, maxTTFT float64
	var waitingRole, ttftRole string
	for _, role := range result.Roles {
		if role.WaitingP50 > maxWaiting {
			maxWaiting, waitingRole = role.WaitingP50, role.Role
		}
		if role.TTFTP99 > maxTTFT {
			maxTTFT, ttftRole = role.TTFTP99, role.Role
		}
	}
	if maxWaiting >= condition.WaitingRequestsP50 && maxTTFT >= condition.TTFTP99.Seconds() {
		result.Reasons = append(result.Reasons, fmt.Sprintf(
			"queueing: waiting p50 %.0f on %s (threshold %.0f), TTFT p99 %s on %s (threshold %s)",
			maxWaiting, waitingRole, condition.WaitingRequestsP50,
			time.Duration(maxTTFT*float64(time.Second)), ttftRole, condition.TTFTP99))
	}
	for _, role := range result.Roles {
		if role.KVUsageMax >= condition.KVCacheUsage && role.Preemptions >= condition.Preemptions {
			result.Reasons = append(result.Reasons, fmt.Sprintf(
				"KV cache on %s: usage %.1f%% (threshold %.1f%%), preemptions %.0f (threshold %.0f)",
				role.Role, role.KVUsageMax*100, condition.KVCacheUsage*100,
				role.Preemptions, condition.Preemptions))
		}
	}
	result.Congested = len(result.Reasons) > 0

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	return result, errors.Join(errs...)
}
