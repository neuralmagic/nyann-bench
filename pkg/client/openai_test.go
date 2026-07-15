package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
