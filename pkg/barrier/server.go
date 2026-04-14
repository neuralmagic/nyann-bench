package barrier

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

// Server is an HTTP barrier server that coordinates synchronized start times
// across multiple benchmark pods. Pod-0 (leader) runs the server; all pods
// POST /barrier/ready to register and block until all workers arrive.
type Server struct {
	nWorkers int
	port     int

	mu       sync.Mutex
	barriers map[int]*barrierState // barrier_id -> state
}

type barrierState struct {
	mu    sync.Mutex
	ready map[int]chan time.Time // worker_id -> response channel
}

type readyRequest struct {
	WorkerID  int `json:"worker_id"`
	BarrierID int `json:"barrier_id"`
}

type readyResponse struct {
	StartTime string `json:"start_time"` // RFC3339Nano
}

// NewServer creates a barrier server that waits for nWorkers at each barrier.
func NewServer(nWorkers, port int) *Server {
	return &Server{
		nWorkers: nWorkers,
		port:     port,
		barriers: make(map[int]*barrierState),
	}
}

// ListenAndServe starts the barrier HTTP server. It blocks until the context
// is cancelled or an error occurs.
func (s *Server) ListenAndServe(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /barrier/ready", s.handleReady)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
		BaseContext: func(_ net.Listener) context.Context {
			return ctx
		},
	}

	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	slog.Info("Barrier server listening", "port", s.port, "workers", s.nWorkers)
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) getBarrier(id int) *barrierState {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.barriers[id]
	if !ok {
		b = &barrierState{
			ready: make(map[int]chan time.Time),
		}
		s.barriers[id] = b
	}
	return b
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	var req readyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	b := s.getBarrier(req.BarrierID)

	b.mu.Lock()
	if _, exists := b.ready[req.WorkerID]; exists {
		b.mu.Unlock()
		http.Error(w, fmt.Sprintf("worker %d already registered for barrier %d", req.WorkerID, req.BarrierID), http.StatusConflict)
		return
	}

	ch := make(chan time.Time, 1)
	b.ready[req.WorkerID] = ch
	allReady := len(b.ready) == s.nWorkers

	if allReady {
		// All workers are here — compute start time and notify everyone
		startTime := time.Now().Add(1 * time.Second)
		for _, wch := range b.ready {
			wch <- startTime
		}
		slog.Info("Barrier released", "barrier_id", req.BarrierID, "start_time", startTime.Format(time.RFC3339Nano))
	}
	b.mu.Unlock()

	// Block until start time is broadcast (or context cancelled)
	select {
	case t := <-ch:
		resp := readyResponse{StartTime: t.Format(time.RFC3339Nano)}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	case <-r.Context().Done():
		http.Error(w, "request cancelled", http.StatusServiceUnavailable)
	}
}
