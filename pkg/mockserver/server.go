package mockserver

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"time"
)

type Server struct {
	Addr            string
	TTFT            time.Duration
	ITL             time.Duration
	OutputTokens    int
	Model           string
	ResponseContent string // If set, stream this as the response content instead of "tok " filler
}

type chatRequest struct {
	Model     string          `json:"model"`
	Messages  json.RawMessage `json:"messages"`
	Stream    bool            `json:"stream"`
	MaxTokens int             `json:"max_tokens,omitempty"`
}

func (s *Server) ListenAndServe() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return http.ListenAndServe(s.Addr, mux)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": s.Model, "object": "model"},
		},
	})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	outputTokens := s.OutputTokens
	if req.MaxTokens > 0 {
		outputTokens = req.MaxTokens
	}

	model := req.Model
	if model == "" {
		model = s.Model
	}

	if !req.Stream {
		s.handleNonStreaming(w, model, outputTokens)
		return
	}
	s.handleStreaming(w, model, outputTokens)
}

func (s *Server) handleNonStreaming(w http.ResponseWriter, model string, outputTokens int) {
	time.Sleep(s.TTFT + s.ITL*time.Duration(outputTokens-1))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", rand.Int63()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": filler(outputTokens)},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"completion_tokens": outputTokens,
			"total_tokens":      outputTokens,
		},
	})
}

func (s *Server) handleStreaming(w http.ResponseWriter, model string, outputTokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	id := fmt.Sprintf("chatcmpl-%d", rand.Int63())

	// Simulate TTFT
	time.Sleep(s.TTFT)

	// Build token chunks
	chunks := make([]string, outputTokens)
	if s.ResponseContent != "" {
		// Split response content across chunks
		runes := []rune(s.ResponseContent)
		chunkSize := (len(runes) + outputTokens - 1) / outputTokens
		for i := 0; i < outputTokens; i++ {
			start := i * chunkSize
			end := start + chunkSize
			if start >= len(runes) {
				chunks[i] = ""
			} else {
				if end > len(runes) {
					end = len(runes)
				}
				chunks[i] = string(runes[start:end])
			}
		}
	} else {
		for i := range chunks {
			chunks[i] = "tok "
		}
	}

	for i := 0; i < outputTokens; i++ {
		if i > 0 {
			time.Sleep(s.ITL)
		}

		writeSSE(w, flusher, map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]string{"content": chunks[i]},
			}},
		})
	}

	// Final chunk with finish_reason + usage
	writeSSE(w, flusher, map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]string{},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"completion_tokens": outputTokens,
			"total_tokens":      outputTokens,
		},
	})

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func writeSSE(w http.ResponseWriter, f http.Flusher, v any) {
	data, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", data)
	f.Flush()
}

func filler(tokens int) string {
	b := make([]byte, 0, tokens*4)
	for i := 0; i < tokens; i++ {
		b = append(b, "tok "...)
	}
	return string(b)
}
