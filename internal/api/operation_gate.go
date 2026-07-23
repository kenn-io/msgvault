package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const cliRunGateInspectionMaxBytes = 1 << 20

// operationGateWaitLimit bounds how long a gated request queues server-side
// before it is turned away with an operation_in_progress error naming the
// current holder. Clients retry, so short gate holds stay invisible while
// long ones surface who is blocking instead of hanging silently. Variable
// only so tests can shorten it.
var operationGateWaitLimit = 10 * time.Second

var errCLIRunGateInspectionBodyTooLarge = errors.New("cli run request body is too large to inspect before routing")

// OperationGate serializes daemon-owned mutating work.
type OperationGate interface {
	BeginWork() (func(), bool)
	BeginWorkContext(ctx context.Context) (func(), bool)
}

// LabeledOperationGate is implemented by gates that can report what is
// currently holding them, so waiters can be told what they are waiting for
// and background holders can be told a request is waiting.
type LabeledOperationGate interface {
	OperationGate
	BeginLabeledWorkContext(ctx context.Context, label string) (func(), bool)
	BeginRequestWorkContext(ctx context.Context, label string) (func(), bool)
	Holder() (label string, since time.Time, held bool)
	HasRequestWaiters() bool
	Draining() bool
}

type SerialOperationGate struct {
	initOnce sync.Once
	sem      chan struct{}
	mu       sync.Mutex
	drainCh  chan struct{}
	draining bool
	active   int

	holderLabel    string
	holderSince    time.Time
	requestWaiters int
}

func NewSerialOperationGate() *SerialOperationGate {
	return &SerialOperationGate{}
}

func (g *SerialOperationGate) BeginWork() (func(), bool) {
	return g.BeginWorkContext(context.Background())
}

func (g *SerialOperationGate) BeginWorkContext(ctx context.Context) (func(), bool) {
	return g.BeginLabeledWorkContext(ctx, "")
}

// BeginRequestWorkContext is BeginLabeledWorkContext for API-request work.
// While queued, the request counts toward HasRequestWaiters so background
// holders (scheduled syncs, embed passes) know to yield.
func (g *SerialOperationGate) BeginRequestWorkContext(ctx context.Context, label string) (func(), bool) {
	if g != nil {
		g.mu.Lock()
		g.requestWaiters++
		g.mu.Unlock()
		defer func() {
			g.mu.Lock()
			g.requestWaiters--
			g.mu.Unlock()
		}()
	}
	return g.BeginLabeledWorkContext(ctx, label)
}

// HasRequestWaiters reports whether an API request is queued on the gate.
func (g *SerialOperationGate) HasRequestWaiters() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requestWaiters > 0
}

func (g *SerialOperationGate) BeginLabeledWorkContext(ctx context.Context, label string) (func(), bool) {
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
		g.holderLabel = label
		g.holderSince = time.Now()
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
			g.holderLabel = ""
			g.holderSince = time.Time{}
			g.mu.Unlock()
			<-sem
		})
	}, true
}

// Holder reports what currently holds the gate, if anything.
func (g *SerialOperationGate) Holder() (string, time.Time, bool) {
	if g == nil {
		return "", time.Time{}, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active == 0 {
		return "", time.Time{}, false
	}
	return g.holderLabel, g.holderSince, true
}

// Draining reports whether the gate is rejecting new work for shutdown.
func (g *SerialOperationGate) Draining() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.draining
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

// operationGateMiddleware serializes mutating requests behind the operation
// gate. authorized guards gate state from unauthenticated callers: requests
// that fail it pass straight through — without registering as request
// waiters, triggering scheduler yields, or observing operation state — and
// are rejected by the API auth layer below. A nil authorized gates every
// request.
func operationGateMiddleware(gate OperationGate, authorized func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if gate == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if authorized != nil && !authorized(r) {
				next.ServeHTTP(w, r)
				return
			}
			shouldGate, label, err := operationGateRequest(r)
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
			done, ok := beginGateWorkBounded(r.Context(), gate, label)
			if !ok {
				writeOperationGateBusy(w, gate)
				return
			}
			defer done()
			next.ServeHTTP(w, r)
		})
	}
}

// beginGateWorkBounded queues on the gate for at most operationGateWaitLimit,
// so short holds are absorbed invisibly and long ones fail fast to the busy
// response instead of hanging the client with no feedback.
func beginGateWorkBounded(ctx context.Context, gate OperationGate, label string) (func(), bool) {
	waitCtx, cancel := context.WithTimeout(ctx, operationGateWaitLimit)
	defer cancel()
	if lg, ok := gate.(LabeledOperationGate); ok {
		return lg.BeginRequestWorkContext(waitCtx, label)
	}
	return gate.BeginWorkContext(waitCtx)
}

func writeOperationGateBusy(w http.ResponseWriter, gate OperationGate) {
	lg, ok := gate.(LabeledOperationGate)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "server_busy", "server is busy or shutting down")
		return
	}
	if lg.Draining() {
		writeError(w, http.StatusServiceUnavailable, "server_busy", "server is shutting down")
		return
	}
	message := "another operation is running"
	if label, since, held := lg.Holder(); held && label != "" {
		message = fmt.Sprintf("%s has been running for %s",
			label, time.Since(since).Round(time.Second))
	}
	writeError(w, http.StatusServiceUnavailable, "operation_in_progress", message)
}

// operationGateExemptPaths are non-GET endpoints that do not mutate the
// archive: they must not queue behind long archive operations. Most only read;
// the session endpoints mutate process-local authentication state. Verify is
// NOT exempt: its subprocess opens the store read-write and runs schema
// init/migrations.
//
// The backup freeze endpoints are exempt for a different reason: begin
// acquires the operation gate itself (see beginLabeledOperationGateWork in
// handleBackupFreezeBegin), so routing them through the generic middleware
// gate as well would deadlock begin against its own acquisition.
var operationGateExemptPaths = map[string]bool{
	queryEndpointPath:                true,
	sessionPath:                      true,
	sessionLoginPath:                 true,
	"/api/v1/cli/add-calendar/plan":  true,
	"/api/v1/cli/delete-staged/plan": true,
	"/api/v1/cli/embeddings/plan":    true,
	"/api/v1/cli/deduplicate/plan":   true,
	backupFreezeBeginPath:            true,
	backupFreezeEndPath:              true,
}

// readOnlyPostRoutePatterns lists the analytical POST routes whose handlers
// only read committed archive state (plus process-local in-memory state such
// as explore candidate snapshots and preflight operation tokens). They use
// POST solely because their requests are structured predicate bodies, so
// queueing them on the operation gate would serialize pure reads behind long
// archive mutations. Reads never needed the gate: every GET already bypasses
// it and relies on the committed-cache revision/snapshot machinery for
// consistency instead.
//
// The classification stays deny-by-default: a POST route not listed here
// still gates. TestReadOnlyPostRoutePatternsMatchExplorationRoutes keeps this
// table in sync with the routes registered through registerExploreRoute and
// registerSearchCoverageRoute (the OpenAPI "Exploration" tag), so a new
// analytical route forces a conscious classification decision here.
//
// The remote-image proxy is the one non-Exploration entry: it is POST only
// so the session CSRF middleware treats it as an unsafe method (same-origin
// plus X-Csrf-Token), and its handler touches no archive state at all — it
// performs one SSRF-validated outbound fetch — so it must stay available
// while a long archive operation holds the gate.
var readOnlyPostRoutePatterns = []string{
	remoteImagePath,
	"/api/v1/explore",
	"/api/v1/explore/groups",
	"/api/v1/explore/preflight",
	"/api/v1/explore/match-counts",
	"/api/v1/explore/files",
	"/api/v1/files/search",
	"/api/v1/files/groups",
	"/api/v1/people/search",
	"/api/v1/people/{id}/summary",
	"/api/v1/people/{id}/timeline",
	"/api/v1/people/{id}/files/search",
	"/api/v1/domains/search",
	"/api/v1/domains/{domain}/summary",
	"/api/v1/domains/{domain}/timeline",
	"/api/v1/domains/{domain}/files/search",
	"/api/v1/relationships",
	"/api/v1/relationships/{id}/timeline",
	"/api/v1/search/coverage",
}

var (
	readOnlyPostRouteOnce sync.Once
	readOnlyPostRouteMux  *http.ServeMux
)

// readOnlyPostRouteRequest reports whether r targets one of the read-only
// analytical POST routes. Matching goes through a net/http ServeMux built
// from readOnlyPostRoutePatterns so the path-parameter routes ({id},
// {domain}) match with the same semantics as the API router itself.
func readOnlyPostRouteRequest(r *http.Request) bool {
	readOnlyPostRouteOnce.Do(func() {
		readOnlyPostRouteMux = http.NewServeMux()
		for _, pattern := range readOnlyPostRoutePatterns {
			readOnlyPostRouteMux.Handle("POST "+pattern, http.NotFoundHandler())
		}
	})
	_, pattern := readOnlyPostRouteMux.Handler(r)
	return pattern != ""
}

func operationGateRequest(r *http.Request) (bool, string, error) {
	if r.URL.Path == DaemonShutdownPath {
		return false, "", nil
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false, "", nil
	}
	if operationGateExemptPaths[r.URL.Path] {
		return false, "", nil
	}
	if readOnlyPostRouteRequest(r) {
		return false, "", nil
	}
	if r.URL.Path == "/api/v1/cli/run" {
		label, skip, err := cliRunGateDecision(r)
		if err != nil {
			return false, "", err
		}
		if skip {
			return false, "", nil
		}
		return true, label, nil
	}
	return true, operationGateLabelFromPath(r.URL.Path), nil
}

// cliRunReadOnlyCommands are proxied CLI commands that only read. Keys are
// the leading command-path words of CLIRunRequest args (flags follow them).
var cliRunReadOnlyCommands = map[string]bool{
	"logs":            true,
	"list-deletions":  true,
	"show-deletion":   true,
	"embeddings list": true,
}

// cliRunSelfGatedCommands are proxied CLI commands that acquire the
// operation gate themselves instead of relying on the middleware: "backup
// create" brackets its own work with the backup freeze begin/end endpoints
// (see handleBackupFreezeBegin), so the middleware must skip it exactly like
// a read-only command or the two acquisitions would deadlock each other.
var cliRunSelfGatedCommands = map[string]bool{
	"backup create": true,
}

func cliRunGateDecision(r *http.Request) (label string, skip bool, err error) {
	if r == nil || r.Body == nil {
		return "", false, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, cliRunGateInspectionMaxBytes+1))
	if err != nil {
		return "", false, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if len(body) > cliRunGateInspectionMaxBytes {
		return "", false, errCLIRunGateInspectionBodyTooLarge
	}

	// An unparseable body stays gated with a generic label; the handler
	// produces the real decode error for the client.
	var req struct {
		Args []string `json:"args"`
	}
	if json.Unmarshal(body, &req) == nil && len(req.Args) > 0 {
		command := cliRunCommandWords(req.Args)
		if cliRunReadOnlyCommands[command] || cliRunSelfGatedCommands[command] {
			return "", true, nil
		}
		if command != "" {
			return "msgvault " + command, false, nil
		}
	}
	return "msgvault CLI command", false, nil
}

// cliRunCommandGroups are proxied parent commands whose second arg is a
// subcommand name rather than a positional value.
var cliRunCommandGroups = map[string]bool{
	"embeddings": true,
	"backup":     true,
}

// cliRunCommandWords extracts the command path from proxied args. Positional
// values and flags follow the command path, so only a known group consumes a
// second word.
func cliRunCommandWords(args []string) string {
	first := args[0]
	if strings.HasPrefix(first, "-") {
		return ""
	}
	if cliRunCommandGroups[first] && len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		return first + " " + args[1]
	}
	return first
}

func operationGateLabelFromPath(urlPath string) string {
	name := strings.TrimPrefix(urlPath, "/api/v1/")
	name = strings.TrimPrefix(name, "cli/")
	if name == "" {
		return "an API request"
	}
	return "msgvault " + name
}

// beginBackgroundOperationGateWork acquires the gate for daemon-owned
// background work: unlike beginLabeledOperationGateWork it does not count as
// a request waiter, so gate holders that yield to requests (scheduled syncs,
// embed passes) are not interrupted by it.
func (s *Server) beginBackgroundOperationGateWork(ctx context.Context, label string) (func(), bool) {
	if s.operationGate == nil {
		return func() {}, true
	}
	if lg, ok := s.operationGate.(LabeledOperationGate); ok {
		return lg.BeginLabeledWorkContext(ctx, label)
	}
	return s.operationGate.BeginWorkContext(ctx)
}

func (s *Server) beginLabeledOperationGateWork(ctx context.Context, label string) (func(), bool) {
	if s.operationGate == nil {
		return func() {}, true
	}
	if lg, ok := s.operationGate.(LabeledOperationGate); ok {
		// Request work, not plain labeled work: this runs on behalf of an
		// API request, so a scheduled job holding the gate must see it as a
		// waiter and yield.
		return lg.BeginRequestWorkContext(ctx, label)
	}
	return s.operationGate.BeginWorkContext(ctx)
}
