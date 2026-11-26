package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Server wires HTTP routes and handlers.
type Server struct {
	log *slog.Logger
	mux *http.ServeMux
}

// New creates a server with registered routes.
func New(log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{log: log, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /health", s.handleHealth)
	return s
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	return s.mux
}

type healthResponse struct {
	Status string `json:"status"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(healthResponse{Status: "ok"}); err != nil {
		s.log.ErrorContext(r.Context(), "encode health response", "error", err)
	}
}
