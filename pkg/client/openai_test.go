package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientDoesNotFollowRedirects(t *testing.T) {
	reached := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"should-not-be-read"}]}`))
	}))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	if _, err := New(redirect.URL).DetectModel(context.Background()); err == nil {
		t.Fatal("redirect response unexpectedly produced a model")
	}
	if reached {
		t.Fatal("client followed a redirect outside the selected target")
	}
}

func TestChatStreamSeparatesReasoningFromGeneratedText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning\":\"think \"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"more \"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	c := New(server.URL + "/v1")
	result := c.ChatStream(context.Background(), &Request{
		Model:    "test-model",
		Messages: []Message{{Role: "user", Content: "question"}},
	})

	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.Content != "answer" {
		t.Fatalf("Content = %q, want %q", result.Content, "answer")
	}
	if result.Reasoning != "think more " {
		t.Fatalf("Reasoning = %q, want %q", result.Reasoning, "think more ")
	}
	if result.GeneratedText != "think more answer" {
		t.Fatalf("GeneratedText = %q, want %q", result.GeneratedText, "think more answer")
	}
	if len(result.TokenTimes) != 3 {
		t.Fatalf("TokenTimes has %d entries, want 3", len(result.TokenTimes))
	}
}

// usageServer requires include_usage, then streams one token and the usage.
func usageServer(t *testing.T, usage string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			StreamOptions *struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.StreamOptions == nil || !body.StreamOptions.IncludeUsage {
			http.Error(w, "stream_options.include_usage not set", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"text\":\"hi\",\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":%s}\n\n", usage)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server
}

func TestChatStreamParsesCachedPromptTokens(t *testing.T) {
	server := usageServer(t, `{"prompt_tokens":100,"completion_tokens":1,"total_tokens":101,"prompt_tokens_details":{"cached_tokens":96}}`)

	result := New(server.URL+"/v1").ChatStream(context.Background(), &Request{
		Model:         "test-model",
		Messages:      []Message{{Role: "user", Content: "question"}},
		StreamOptions: map[string]any{"include_usage": true},
	})
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.Usage == nil || result.Usage.PromptTokens != 100 {
		t.Fatalf("Usage = %+v, want prompt_tokens 100", result.Usage)
	}
	if result.Usage.PromptTokensDetails == nil || result.Usage.PromptTokensDetails.CachedTokens != 96 {
		t.Fatalf("PromptTokensDetails = %+v, want cached_tokens 96", result.Usage.PromptTokensDetails)
	}
}

func TestChatStreamWithoutPromptTokensDetails(t *testing.T) {
	server := usageServer(t, `{"prompt_tokens":100,"completion_tokens":1,"total_tokens":101}`)

	result := New(server.URL+"/v1").ChatStream(context.Background(), &Request{
		Model:         "test-model",
		Messages:      []Message{{Role: "user", Content: "question"}},
		StreamOptions: map[string]any{"include_usage": true},
	})
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.Usage == nil {
		t.Fatal("Usage = nil, want the usage chunk parsed")
	}
	if result.Usage.PromptTokensDetails != nil {
		t.Fatalf("PromptTokensDetails = %+v, want nil when the server does not report it", result.Usage.PromptTokensDetails)
	}
}

func TestCompletionStreamParsesUsage(t *testing.T) {
	server := usageServer(t, `{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}`)

	result := New(server.URL+"/v1").CompletionStream(context.Background(), &CompletionRequest{
		Model:         "test-model",
		Prompt:        "question",
		StreamOptions: map[string]any{"include_usage": true},
	})
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.Usage == nil || result.Usage.PromptTokens != 10 {
		t.Fatalf("Usage = %+v, want prompt_tokens 10", result.Usage)
	}
}
