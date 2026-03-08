package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	Stream    bool      `json:"stream"`
	MaxTokens int       `json:"max_tokens,omitempty"`
}

type CompletionRequest struct {
	Model       string    `json:"model"`
	Prompt      string    `json:"prompt"`
	Stream      bool      `json:"stream"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stop        []string  `json:"stop,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
}

type TokenEvent struct {
	Content  string
	Time     time.Time
	IsFirst  bool
	IsFinal  bool
	Usage    *Usage
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type Result struct {
	RequestStart time.Time
	FirstToken   time.Time
	TokenTimes   []time.Time // Time of each token arrival
	EndTime      time.Time
	Content      string
	Usage        *Usage
	Err          error
}

func (r *Result) TTFT() time.Duration {
	if r.FirstToken.IsZero() {
		return 0
	}
	return r.FirstToken.Sub(r.RequestStart)
}

func (r *Result) ITLs() []time.Duration {
	if len(r.TokenTimes) < 2 {
		return nil
	}
	itls := make([]time.Duration, len(r.TokenTimes)-1)
	for i := 1; i < len(r.TokenTimes); i++ {
		itls[i-1] = r.TokenTimes[i].Sub(r.TokenTimes[i-1])
	}
	return itls
}

func (r *Result) TotalLatency() time.Duration {
	return r.EndTime.Sub(r.RequestStart)
}

func (r *Result) OutputTokens() int {
	if r.Usage != nil {
		return r.Usage.CompletionTokens
	}
	return len(r.TokenTimes)
}

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL:    strings.TrimRight(baseURL, "/"),
		HTTPClient: &http.Client{},
	}
}

// DetectModel queries /v1/models and returns the first model ID.
func (c *Client) DetectModel(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/models", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("querying %s/models: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()

	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("parsing /v1/models response: %w", err)
	}
	if len(result.Data) == 0 {
		return "", fmt.Errorf("no models found at %s/models", c.BaseURL)
	}
	return result.Data[0].ID, nil
}

// CalibrateTokenRatio sends a sample text to /tokenize and returns the
// measured chars-per-token ratio. Falls back to 4.0 if the endpoint is unavailable.
func (c *Client) CalibrateTokenRatio(ctx context.Context, sample string, model string) (float64, error) {
	if len(sample) < 100 {
		return 4.0, nil
	}
	// Use a ~2000 char sample for calibration
	if len(sample) > 2000 {
		sample = sample[:2000]
	}

	body, err := json.Marshal(map[string]any{
		"model":  model,
		"prompt": sample,
	})
	if err != nil {
		return 4.0, err
	}

	// Try /tokenize (vLLM endpoint, not under /v1)
	baseURL := strings.TrimSuffix(c.BaseURL, "/v1")
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/tokenize", bytes.NewReader(body))
	if err != nil {
		return 4.0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("calling /tokenize: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("/tokenize returned status %d", resp.StatusCode)
	}

	var result struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("parsing /tokenize response: %w", err)
	}

	if result.Count == 0 {
		return 0, fmt.Errorf("/tokenize returned 0 tokens")
	}

	ratio := float64(len(sample)) / float64(result.Count)
	return ratio, nil
}

// ChatStream sends a streaming chat completion request and returns a Result
// with token-level timing.
func (c *Client) ChatStream(ctx context.Context, req *Request) *Result {
	req.Stream = true
	result := &Result{RequestStart: time.Now()}

	body, err := json.Marshal(req)
	if err != nil {
		result.Err = fmt.Errorf("marshaling request: %w", err)
		result.EndTime = time.Now()
		return result
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		result.Err = fmt.Errorf("creating request: %w", err)
		result.EndTime = time.Now()
		return result
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		result.Err = fmt.Errorf("sending request: %w", err)
		result.EndTime = time.Now()
		return result
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		result.Err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
		result.EndTime = time.Now()
		return result
	}

	var content strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	// Increase scanner buffer for large responses
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		now := time.Now()

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *Usage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // Skip malformed chunks
		}

		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			if result.FirstToken.IsZero() {
				result.FirstToken = now
			}
			result.TokenTimes = append(result.TokenTimes, now)
			content.WriteString(chunk.Choices[0].Delta.Content)
		}

		if chunk.Usage != nil {
			result.Usage = chunk.Usage
		}
	}

	result.Content = content.String()
	result.EndTime = time.Now()

	if err := scanner.Err(); err != nil {
		result.Err = fmt.Errorf("reading stream: %w", err)
	}

	return result
}

// CompletionStream sends a streaming completion request to /v1/completions
// and returns a Result with token-level timing.
func (c *Client) CompletionStream(ctx context.Context, req *CompletionRequest) *Result {
	req.Stream = true
	result := &Result{RequestStart: time.Now()}

	body, err := json.Marshal(req)
	if err != nil {
		result.Err = fmt.Errorf("marshaling request: %w", err)
		result.EndTime = time.Now()
		return result
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+"/completions", bytes.NewReader(body))
	if err != nil {
		result.Err = fmt.Errorf("creating request: %w", err)
		result.EndTime = time.Now()
		return result
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		result.Err = fmt.Errorf("sending request: %w", err)
		result.EndTime = time.Now()
		return result
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		result.Err = fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
		result.EndTime = time.Now()
		return result
	}

	var content strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		now := time.Now()

		var chunk struct {
			Choices []struct {
				Text         string  `json:"text"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *Usage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if len(chunk.Choices) > 0 && chunk.Choices[0].Text != "" {
			if result.FirstToken.IsZero() {
				result.FirstToken = now
			}
			result.TokenTimes = append(result.TokenTimes, now)
			content.WriteString(chunk.Choices[0].Text)
		}

		if chunk.Usage != nil {
			result.Usage = chunk.Usage
		}
	}

	result.Content = content.String()
	result.EndTime = time.Now()

	if err := scanner.Err(); err != nil {
		result.Err = fmt.Errorf("reading stream: %w", err)
	}

	return result
}
