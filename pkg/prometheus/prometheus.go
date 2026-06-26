package prometheus

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"
)

type Client struct {
	BaseURL string
	HTTP    *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

type DataPoint struct {
	Timestamp float64
	Value     float64
}

type LatencyStats struct {
	P50 float64 `json:"p50"`
	P99 float64 `json:"p99"`
}

type GaugeStats struct {
	P50 float64 `json:"p50"`
	Max float64 `json:"max"`
}

func (c *Client) QueryRange(query string, start, end time.Time, step time.Duration) ([]DataPoint, error) {
	params := url.Values{}
	params.Set("query", query)
	params.Set("start", fmt.Sprintf("%d", start.Unix()))
	params.Set("end", fmt.Sprintf("%d", end.Unix()))
	params.Set("step", fmt.Sprintf("%g", step.Seconds()))

	resp, err := c.HTTP.Get(c.BaseURL + "/api/v1/query_range?" + params.Encode())
	if err != nil {
		return nil, fmt.Errorf("prometheus range query: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	var promResp struct {
		Data struct {
			Result []struct {
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &promResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	if len(promResp.Data.Result) == 0 {
		return nil, nil
	}

	var points []DataPoint
	for _, pair := range promResp.Data.Result[0].Values {
		if len(pair) < 2 {
			continue
		}
		var ts float64
		if err := json.Unmarshal(pair[0], &ts); err != nil {
			continue
		}
		var valStr string
		if err := json.Unmarshal(pair[1], &valStr); err != nil {
			continue
		}
		val, err := strconv.ParseFloat(valStr, 64)
		if err != nil || math.IsNaN(val) || math.IsInf(val, 0) {
			continue
		}
		points = append(points, DataPoint{Timestamp: ts, Value: val})
	}
	return points, nil
}

// HistogramQuantile queries a Prometheus histogram bucket for P50 and P99 over a time window.
// The bucket metric should be a histogram bucket (e.g. "vllm:time_to_first_token_seconds_bucket").
// The podFilter is used as a regex match on the pod label (e.g. "my-deploy-.*").
func (c *Client) HistogramQuantile(bucket, podFilter string, start, end time.Time) (LatencyStats, error) {
	window := int(end.Sub(start).Seconds())
	if window < 1 {
		return LatencyStats{}, nil
	}
	windowStr := fmt.Sprintf("%ds", window)

	quantiles := []float64{0.50, 0.99}
	vals := make([]float64, 2)

	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once

	wg.Add(2)
	for i, q := range quantiles {
		go func(idx int, quantile float64) {
			defer wg.Done()
			query := fmt.Sprintf(
				`histogram_quantile(%g, sum(increase(%s{pod=~"%s"}[%s])) by (le))`,
				quantile, bucket, podFilter, windowStr,
			)
			points, err := c.QueryRange(query, end, end, time.Second)
			if err != nil {
				errOnce.Do(func() { firstErr = err })
				return
			}
			if len(points) > 0 {
				vals[idx] = points[0].Value
			}
		}(i, q)
	}
	wg.Wait()
	if firstErr != nil {
		return LatencyStats{}, firstErr
	}
	return LatencyStats{P50: vals[0], P99: vals[1]}, nil
}

// QueryGaugeStats queries a Prometheus gauge over a time range and returns P50 and max.
func (c *Client) QueryGaugeStats(query string, start, end time.Time) (GaugeStats, error) {
	points, err := c.QueryRange(query, start, end, 5*time.Second)
	if err != nil {
		return GaugeStats{}, err
	}
	if len(points) == 0 {
		return GaugeStats{}, nil
	}
	vals := make([]float64, len(points))
	for i, p := range points {
		vals[i] = p.Value
	}
	sort.Float64s(vals)
	return GaugeStats{
		P50: percentile(vals, 0.50),
		Max: vals[len(vals)-1],
	}, nil
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := p * float64(len(sorted)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi || hi >= len(sorted) {
		return sorted[lo]
	}
	frac := idx - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}
