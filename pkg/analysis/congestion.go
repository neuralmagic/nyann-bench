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
	WaitingMax  float64
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
// prefill/decode deployment. The queueing and cache signal pairs may span
// roles: for example, a prefill queue and decoder-observed TTFT together are a
// valid end-to-end congestion signal.
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
			stats, err := client.QueryGaugeStats(fmt.Sprintf(`max(vllm:num_requests_waiting{%s})`, labels), start, end)
			if err != nil {
				errCh <- fmt.Errorf("%s waiting requests: %w", target.role, err)
				return
			}
			*dst = stats.Max
		}(&result.Roles[i].WaitingMax)
		go func(dst *float64) {
			defer wg.Done()
			stats, err := client.HistogramQuantile("vllm:time_to_first_token_seconds_bucket", target.pods, target.job, start, end)
			if err != nil {
				errCh <- fmt.Errorf("%s TTFT: %w", target.role, err)
				return
			}
			*dst = stats.P99
		}(&result.Roles[i].TTFTP99)
		go func(dst *float64) {
			defer wg.Done()
			stats, err := client.QueryGaugeStats(fmt.Sprintf(`max(vllm:kv_cache_usage_perc{%s})`, labels), start, end)
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

	var maxWaiting, maxTTFT, maxKV, maxPreemptions float64
	var waitingRole, ttftRole, kvRole, preemptionRole string
	for _, role := range result.Roles {
		if role.WaitingMax > maxWaiting {
			maxWaiting, waitingRole = role.WaitingMax, role.Role
		}
		if role.TTFTP99 > maxTTFT {
			maxTTFT, ttftRole = role.TTFTP99, role.Role
		}
		if role.KVUsageMax > maxKV {
			maxKV, kvRole = role.KVUsageMax, role.Role
		}
		if role.Preemptions > maxPreemptions {
			maxPreemptions, preemptionRole = role.Preemptions, role.Role
		}
	}
	if maxWaiting >= condition.WaitingRequests && maxTTFT >= condition.TTFT.Seconds() {
		result.Reasons = append(result.Reasons, fmt.Sprintf(
			"queueing: waiting %.0f on %s (threshold %.0f), TTFT p99 %s on %s (threshold %s)",
			maxWaiting, waitingRole, condition.WaitingRequests,
			time.Duration(maxTTFT*float64(time.Second)), ttftRole, condition.TTFT))
	}
	if maxKV >= condition.KVCacheUsage && maxPreemptions >= condition.Preemptions {
		result.Reasons = append(result.Reasons, fmt.Sprintf(
			"KV cache: usage %.1f%% on %s (threshold %.1f%%), preemptions %.0f on %s (threshold %.0f)",
			maxKV*100, kvRole, condition.KVCacheUsage*100,
			maxPreemptions, preemptionRole, condition.Preemptions))
	}
	result.Congested = len(result.Reasons) > 0

	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	return result, errors.Join(errs...)
}
