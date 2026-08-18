package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const legacyMCPProtocolVersion = "2025-11-25"

// routeLegacyMCP identifies initialize/session-era traffic without changing
// the existing 2026-07-28 stateless handler. It restores the request body after
// inspecting it so the selected handler sees the original bytes.
func routeLegacyMCP(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodDelete || r.Header.Get("Mcp-Session-Id") != "" {
		return true
	}
	if r.Method != http.MethodPost {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, mcpMaximumRequestBytes+1))
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) > mcpMaximumRequestBytes {
		return false
	}
	var envelope struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return false
	}
	return envelope.Method == "initialize" || envelope.Method == "notifications/initialized"
}

func (s *Server) legacyMCPHandler() http.Handler {
	server := mcp.NewServer(
		&mcp.Implementation{Name: mcpServerName, Version: mcpServerVersion},
		&mcp.ServerOptions{
			Instructions: "Plan before submitting. Use logical targets and bounded reports; raw JSONL and Prometheus payloads are intentionally unavailable through MCP.",
			Capabilities: &mcp.ServerCapabilities{},
		},
	)
	for _, definition := range s.mcpTools() {
		encoded, err := json.Marshal(definition)
		if err != nil {
			panic(fmt.Sprintf("marshal MCP tool %q: %v", definition["name"], err))
		}
		var tool mcp.Tool
		if err := json.Unmarshal(encoded, &tool); err != nil {
			panic(fmt.Sprintf("convert MCP tool %q: %v", definition["name"], err))
		}
		name := tool.Name
		server.AddTool(&tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			value, err := s.callMCPTool(ctx, name, req.Params.Arguments)
			if err != nil {
				return &mcp.CallToolResult{
					Content:           []mcp.Content{&mcp.TextContent{Text: mustMCPJSON(map[string]any{"error": err.Error()})}},
					StructuredContent: map[string]any{"error": err.Error()},
					IsError:           true,
				}, nil
			}
			return &mcp.CallToolResult{
				Content:           []mcp.Content{&mcp.TextContent{Text: mustMCPJSON(value)}},
				StructuredContent: value,
			}, nil
		})
	}

	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			JSONResponse:        true,
			MaxRequestBodyBytes: mcpMaximumRequestBytes,
		},
	)
	return exactLegacyMCPVersion(handler)
}

func exactLegacyMCPVersion(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			http.Error(w, "Origin is not allowed", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
			contentType := strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0])
			if !strings.EqualFold(contentType, "application/json") {
				http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		if r.Header.Get("Mcp-Session-Id") != "" && r.Header.Get("Mcp-Protocol-Version") != legacyMCPProtocolVersion {
			http.Error(w, "Bad Request: unsupported MCP protocol version", http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get("Mcp-Session-Id") == "" {
			body, err := io.ReadAll(io.LimitReader(r.Body, mcpMaximumRequestBytes+1))
			if err != nil || len(body) > mcpMaximumRequestBytes {
				http.Error(w, "Bad Request", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var initialize struct {
				Method string `json:"method"`
				Params struct {
					ProtocolVersion string `json:"protocolVersion"`
				} `json:"params"`
			}
			if json.Unmarshal(body, &initialize) == nil && initialize.Method == "initialize" && initialize.Params.ProtocolVersion != legacyMCPProtocolVersion {
				http.Error(w, "Bad Request: unsupported MCP protocol version", http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func mustMCPJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `{"error":"failed to encode MCP tool result"}`
	}
	if len(encoded) > mcpMaximumResultBytes {
		return fmt.Sprintf(`{"error":"MCP result exceeds the 1 MiB bounded response limit","maximum_bytes":%d}`, mcpMaximumResultBytes)
	}
	return string(encoded)
}
