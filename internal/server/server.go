package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Server wires HTTP routes and handlers.
type Server struct {
	log   *slog.Logger
	mux   *http.ServeMux
	usage UsageSink
	// token is the bearer token the usage route requires. Empty disables it.
	token string
}

// Options carries what the routes read. A nil Usage or an empty Token leaves
// the usage route registered but refusing every report, which is how the
// controller starts before its API is configured.
type Options struct {
	Usage     UsageSink
	AuthToken string
}

// New creates a server with registered routes.
func New(log *slog.Logger, opts Options) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{log: log, mux: http.NewServeMux(), usage: opts.Usage, token: opts.AuthToken}
	s.mux.HandleFunc("GET /health", s.handleHealth)
	s.mux.HandleFunc("POST /v1/usage", s.handleUsage)
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
	s.writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.log.Error("encode response", "error", err)
	}
}
