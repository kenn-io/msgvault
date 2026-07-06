package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"go.kenn.io/msgvault/internal/api"
)

// Vars rather than consts so tests can shorten them. The quiet window keeps
// fast searches free of flicker; only a search that outlives it starts
// showing elapsed time and daemon activity.
var (
	searchStatusQuietWindow = 3 * time.Second
	searchStatusTick        = 2 * time.Second
)

// startSearchStatus prints the transient "Searching..." stderr line and keeps
// it honest while the request runs: once the quiet window passes, the line
// gains elapsed time and — when the daemon reports one via /health — the
// operation the search is actually waiting on (e.g. "checking the search
// index", which can take a minute on a large archive after a daemon
// restart). The returned stop func erases the line; call it before printing
// results.
func startSearchStatus(ctx context.Context, prefix string, info HTTPStoreInfo) func() {
	line := &searchStatusLine{
		out:     os.Stderr,
		prefix:  prefix,
		fetchOp: daemonOperationFetcher(info.URL, httpStoreAPIKey(info)),
		tty: isatty.IsTerminal(os.Stderr.Fd()) ||
			isatty.IsCygwinTerminal(os.Stderr.Fd()),
		start: time.Now(),
	}
	_, _ = fmt.Fprint(line.out, prefix)
	line.width = len(prefix)

	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		line.run(loopCtx)
	}()
	return func() {
		cancel()
		<-done
		line.clear()
	}
}

// searchStatusLine owns one in-place stderr status line. All fields are
// written by the run goroutine only; stop's channel receive orders clear()
// after the final render.
type searchStatusLine struct {
	out     io.Writer
	prefix  string
	fetchOp func(context.Context) *api.OperationHealth
	tty     bool
	start   time.Time
	width   int
	noticed bool
}

func (l *searchStatusLine) run(ctx context.Context) {
	ticker := time.NewTicker(searchStatusTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		elapsed := time.Since(l.start)
		if elapsed < searchStatusQuietWindow {
			continue
		}
		l.render(elapsed, l.fetchOp(ctx))
	}
}

func (l *searchStatusLine) render(elapsed time.Duration, op *api.OperationHealth) {
	if !l.tty {
		// A pipe gets no in-place updates, but the one fact worth a log
		// line — the daemon is busy with something expensive — is still
		// printed once.
		if op != nil && op.Label != "" && !l.noticed {
			l.noticed = true
			_, _ = fmt.Fprintf(l.out, "\nDaemon is busy: %s. The search will finish when it does.\n",
				op.Label)
		}
		return
	}
	line := formatSearchStatus(l.prefix, elapsed, op)
	pad := max(0, l.width-len(line))
	_, _ = fmt.Fprintf(l.out, "\r%s%s", line, strings.Repeat(" ", pad))
	l.width = max(l.width, len(line))
}

// clear erases the status line so results start on a clean line.
func (l *searchStatusLine) clear() {
	_, _ = fmt.Fprintf(l.out, "\r%s\r", strings.Repeat(" ", l.width))
}

// formatSearchStatus renders one status line. The daemon operation label wins
// over a bare elapsed count because it answers the actual question ("why is
// this taking so long"); Busy without a label (unauthenticated /health
// fallback) degrades to the elapsed-only form.
func formatSearchStatus(prefix string, elapsed time.Duration, op *api.OperationHealth) string {
	rounded := elapsed.Round(time.Second)
	if op != nil && op.Label != "" {
		return fmt.Sprintf("%s daemon is busy: %s (%s)", prefix, op.Label, rounded)
	}
	return fmt.Sprintf("%s (%s)", prefix, rounded)
}

// daemonOperationFetcher reports what the daemon is working on, best-effort:
// nil when the daemon is idle, unreachable, or predates operation reporting.
func daemonOperationFetcher(baseURL, apiKey string) func(context.Context) *api.OperationHealth {
	return func(ctx context.Context) *api.OperationHealth {
		health := fetchDaemonHealthWithAPIKey(ctx, baseURL, apiKey)
		if health == nil {
			return nil
		}
		return health.Operation
	}
}

// httpStoreAPIKey returns the API key for the endpoint OpenHTTPStore
// selected, for auxiliary requests (health polling) beside the main client.
func httpStoreAPIKey(info HTTPStoreInfo) string {
	if cfg == nil {
		return ""
	}
	if info.Kind == HTTPStoreConfiguredRemote {
		return cfg.Remote.APIKey
	}
	return cfg.Server.APIKey
}
