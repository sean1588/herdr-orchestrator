package mcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// maxBody bounds a request body: MCP messages are small, and an unbounded read
// would be a trivial memory-exhaustion vector even on loopback.
const maxBody = 1 << 20 // 1 MiB

// Server is the loopback MCP HTTP server. Serve runs the accept loop and returns
// when its context is cancelled (graceful Shutdown); it never blocks the daemon.
type Server struct {
	h *handler
}

// New builds a Server. reader is the read surface (*store.Store), ctrl the
// control surface (*scheduler.Scheduler), and taskID the canonical issue→id
// formatter (engine.TaskID) so the id format is not duplicated here.
func New(reader Reader, ctrl Controller, taskID func(int) string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{h: &handler{reader: reader, ctrl: ctrl, taskID: taskID, log: log}}
}

// Serve runs the HTTP server on ln until ctx is cancelled, then shuts it down
// gracefully. It returns a non-nil error only on an unexpected serve failure.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleHTTP)
	mux.HandleFunc("/", s.handleHTTP)
	srv := &http.Server{Handler: mux}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// handleHTTP reads one JSON-RPC message and writes its response. Every request is
// recover-wrapped so a handler panic never crosses back into the daemon ("no
// panics in the daemon path").
func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	defer func() {
		if rec := recover(); rec != nil {
			s.h.log.Error("mcp handler panic", "recover", rec)
			_, _ = w.Write(mustMarshal(errResp(nil, codeInternal, "internal error")))
		}
	}()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		_, _ = w.Write(mustMarshal(errResp(nil, codeParse, "read error")))
		return
	}
	resp, isNote := s.h.handle(r.Context(), body)
	if isNote {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	_, _ = w.Write(resp)
}
