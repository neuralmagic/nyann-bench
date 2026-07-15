package analysis_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/neuralmagic/nyann-bench/pkg/analysis"
	"github.com/neuralmagic/nyann-bench/pkg/config"
	promclient "github.com/neuralmagic/nyann-bench/pkg/prometheus"
	"github.com/neuralmagic/nyann-bench/pkg/recorder"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCheckStageCongestionChecksPrefillAndDecode(t *testing.T) {
	client := promclient.NewClient("http://prometheus")
	client.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		q := req.URL.Query().Get("query")
		if strings.Contains(q, "num_preemptions_total") {
			if req.URL.Query().Get("start") != "160" || req.URL.Query().Get("end") != "160" {
				t.Errorf("preemptions should cover the full stage at stage end: %s", req.URL.RawQuery)
			}
		} else if strings.Contains(q, "histogram_quantile") {
			if req.URL.Query().Get("start") != "157" || req.URL.Query().Get("end") != "157" || !strings.Contains(q, "[45s]") {
				t.Errorf("TTFT should use the 20%%-95%% histogram window: %s", req.URL.RawQuery)
			}
		} else if req.URL.Query().Get("start") != "112" || req.URL.Query().Get("end") != "157" {
			t.Errorf("steady-state signal should use the 20%%-95%% window: %s", req.URL.RawQuery)
		}
		value := "0"
		switch {
		case strings.Contains(q, "num_requests_waiting") && strings.Contains(q, `job="vllm-prefill"`):
			value = "40"
		case strings.Contains(q, "histogram_quantile") && strings.Contains(q, `job="vllm-decode"`):
			value = "2.5"
		case strings.Contains(q, "kv_cache_usage_perc") && strings.Contains(q, `job="vllm-decode"`):
			value = "0.98"
		case strings.Contains(q, "num_preemptions_total") && strings.Contains(q, `job="vllm-decode"`):
			value = "3"
		}
		body := `{"status":"success","data":{"resultType":"matrix","result":[{"values":[[100,"` + value + `"]]}]}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})

	result, err := analysis.CheckStageCongestion(client, recorder.StageTimestamp{StartTime: 100, EndTime: 160}, "model-decode", config.CongestionCondition{
		WaitingRequestsP50: 32,
		TTFTP99:            2 * time.Second,
		KVCacheUsage:       0.95,
		Preemptions:        2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Congested || len(result.Reasons) != 2 {
		t.Fatalf("expected both congestion signals, got %+v", result)
	}
	if len(result.Roles) != 2 || result.Roles[0].Role != "prefill" || result.Roles[1].Role != "decode" {
		t.Fatalf("expected prefill and decode observations, got %+v", result.Roles)
	}
}

func TestCheckStageCongestionBelowThresholds(t *testing.T) {
	client := promclient.NewClient("http://prometheus")
	client.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"status":"success","data":{"resultType":"matrix","result":[{"values":[[100,"0.1"]]}]}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	result, err := analysis.CheckStageCongestion(client, recorder.StageTimestamp{StartTime: 100, EndTime: 160}, "model", config.CongestionCondition{
		WaitingRequestsP50: 10,
		TTFTP99:            time.Second,
		KVCacheUsage:       0.95,
		Preemptions:        1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Congested {
		t.Fatalf("unexpected congestion: %+v", result)
	}
}

func TestCheckStageCongestionDoesNotMixCacheRoles(t *testing.T) {
	client := promclient.NewClient("http://prometheus")
	client.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		q := req.URL.Query().Get("query")
		value := "0"
		switch {
		case strings.Contains(q, "kv_cache_usage_perc") && strings.Contains(q, `job="vllm-decode"`):
			value = "0.99"
		case strings.Contains(q, "num_preemptions_total") && strings.Contains(q, `job="vllm-prefill"`):
			value = "10"
		}
		body := `{"status":"success","data":{"resultType":"matrix","result":[{"values":[[100,"` + value + `"]]}]}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})

	result, err := analysis.CheckStageCongestion(client, recorder.StageTimestamp{StartTime: 100, EndTime: 160}, "model-decode", config.CongestionCondition{
		WaitingRequestsP50: 100,
		TTFTP99:            10 * time.Second,
		KVCacheUsage:       0.95,
		Preemptions:        1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Congested {
		t.Fatalf("KV usage and preemptions from different roles must not combine: %+v", result)
	}
}
