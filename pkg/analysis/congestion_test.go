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
		value := "0"
		switch {
		case strings.Contains(q, "num_requests_waiting") && strings.Contains(q, `job="vllm-prefill"`):
			value = "40"
		case strings.Contains(q, "histogram_quantile") && strings.Contains(q, `job="vllm-decode"`):
			value = "2.5"
		case strings.Contains(q, "kv_cache_usage_perc") && strings.Contains(q, `job="vllm-decode"`):
			value = "0.98"
		case strings.Contains(q, "num_preemptions_total") && strings.Contains(q, `job="vllm-prefill"`):
			value = "3"
		}
		body := `{"status":"success","data":{"resultType":"matrix","result":[{"values":[[100,"` + value + `"]]}]}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})

	result, err := analysis.CheckStageCongestion(client, recorder.StageTimestamp{StartTime: 100, EndTime: 160}, "model-decode", config.CongestionCondition{
		WaitingRequests: 32,
		TTFT:            2 * time.Second,
		KVCacheUsage:    0.95,
		Preemptions:     2,
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
		WaitingRequests: 10,
		TTFT:            time.Second,
		KVCacheUsage:    0.95,
		Preemptions:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Congested {
		t.Fatalf("unexpected congestion: %+v", result)
	}
}
