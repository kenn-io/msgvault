package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

const cliRunGateInspectionMaxBytes = 1 << 20

var errCLIRunGateInspectionBodyTooLarge = errors.New("cli run request body is too large to inspect before routing")

// OperationGate serializes daemon-owned mutating work.
type OperationGate interface {
	BeginWork() (func(), bool)
	BeginWorkContext(ctx context.Context) (func(), bool)
}

type SerialOperationGate struct {
	initOnce sync.Once
	sem      chan struct{}
	mu       sync.Mutex
	drainCh  chan struct{}
	draining bool
	active   int
}

func NewSerialOperationGate() *SerialOperationGate {
	return &SerialOperationGate{}
}

func (g *SerialOperationGate) BeginWork() (func(), bool) {
	return g.BeginWorkContext(context.Background())
}

func (g *SerialOperationGate) BeginWorkContext(ctx context.Context) (func(), bool) {
	if g == nil {
		return func() {}, true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return func() {}, false
	}
	sem, drainCh := g.state()
	select {
	case sem <- struct{}{}:
		if ctx.Err() != nil {
			<-sem
			return func() {}, false
		}
		g.mu.Lock()
		if g.draining {
			g.mu.Unlock()
			<-sem
			return func() {}, false
		}
		g.active++
		g.mu.Unlock()
	case <-ctx.Done():
		return func() {}, false
	case <-drainCh:
		return func() {}, false
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if g.active > 0 {
				g.active--
			}
			g.mu.Unlock()
			<-sem
		})
	}, true
}

// Drain rejects queued and future work, then waits for active work to finish.
func (g *SerialOperationGate) Drain(ctx context.Context) error {
	if g == nil {
		return nil
	}
	g.StartDrain()
	return g.Wait(ctx)
}

// StartDrain rejects queued and future work. Active work continues until its
// release function runs.
func (g *SerialOperationGate) StartDrain() {
	g.startDrain()
}

// Wait blocks until active work has released the gate.
func (g *SerialOperationGate) Wait(ctx context.Context) error {
	if g == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		g.mu.Lock()
		active := g.active
		g.mu.Unlock()
		if active == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (g *SerialOperationGate) startDrain() {
	_, drainCh := g.state()
	g.mu.Lock()
	if !g.draining {
		g.draining = true
		close(drainCh)
	}
	g.mu.Unlock()
}

func (g *SerialOperationGate) state() (chan struct{}, chan struct{}) {
	g.initOnce.Do(func() {
		g.sem = make(chan struct{}, 1)
		g.drainCh = make(chan struct{})
	})
	return g.sem, g.drainCh
}

func operationGateMiddleware(gate OperationGate) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if gate == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			shouldGate, err := operationGateRequest(r)
			if err != nil {
				if errors.Is(err, errCLIRunGateInspectionBodyTooLarge) {
					writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
						"request body is too large to inspect before routing")
					return
				}
				writeError(w, http.StatusBadRequest, "invalid_request", "request body could not be read")
				return
			}
			if !shouldGate {
				next.ServeHTTP(w, r)
				return
			}
			done, ok := gate.BeginWorkContext(r.Context())
			if !ok {
				writeError(w, http.StatusServiceUnavailable, "server_busy", "server is busy or shutting down")
				return
			}
			defer done()
			next.ServeHTTP(w, r)
		})
	}
}

func operationGateRequest(r *http.Request) (bool, error) {
	if r.URL.Path == DaemonShutdownPath {
		return false, nil
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false, nil
	}
	if r.URL.Path == "/api/v1/cli/run" {
		skip, err := cliRunRequestSkipsOperationGate(r)
		if err != nil {
			return false, err
		}
		if skip {
			return false, nil
		}
	}
	return true, nil
}

func cliRunRequestSkipsOperationGate(r *http.Request) (bool, error) {
	if r == nil || r.Body == nil {
		return false, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, cliRunGateInspectionMaxBytes+1))
	if err != nil {
		return false, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) > cliRunGateInspectionMaxBytes {
		return false, errCLIRunGateInspectionBodyTooLarge
	}

	var req struct {
		Args []string `json:"args"`
	}
	if json.Unmarshal(body, &req) == nil && len(req.Args) > 0 {
		switch req.Args[0] {
		case "logs":
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) beginOperationGateWork(ctx context.Context) (func(), bool) {
	if s.operationGate == nil {
		return func() {}, true
	}
	return s.operationGate.BeginWorkContext(ctx)
}
