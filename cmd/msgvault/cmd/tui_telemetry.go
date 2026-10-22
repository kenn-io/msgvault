package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cenkalti/backoff/v7"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/telemetry"
	apiclient "go.kenn.io/msgvault/pkg/client/generated"
)

// tuiTelemetryFailures is how many failures in a row stop a TUI process's reports.
const tuiTelemetryFailures = 6

// tuiTelemetryGate checks permission once per daemon per UTC day across TUI
// restarts. TUIs running at the same time may each check and may overwrite each
// other's receipts. Reports are sent one at a time; the daemon still owns the
// counts.
type tuiTelemetryGate struct {
	lock   chan struct{}
	path   string
	now    func() time.Time
	policy *backoff.ExponentialBackOff
	state  tuiTelemetryState
	// Failures are shared by every key: a failed report leaves its key free,
	// and the next report after the backoff window checks again. An accepted
	// report clears them.
	failures int
	retryAt  time.Time
	stopped  bool
}

// tuiTelemetryState is the selected daemon's receipts for one UTC day. The
// format version is in the file name, so a new format writes a new file.
type tuiTelemetryState struct {
	Day      string          `json:"day"`
	Disabled bool            `json:"disabled,omitempty"`
	Sent     map[string]bool `json:"sent,omitempty"`
}

func newTUITelemetryGate(dataDir, endpoint string) *tuiTelemetryGate {
	gate := &tuiTelemetryGate{lock: make(chan struct{}, 1), now: time.Now, policy: backoff.NewExponentialBackOff()}
	if dataDir != "" {
		gate.path = filepath.Join(dataDir, fmt.Sprintf("telemetry-tui-v1-%x.json", sha256.Sum256([]byte(endpoint))))
	}
	return gate
}

// report sends one TUI event unless today's receipts or failures rule it out.
func (g *tuiTelemetryGate) report(ctx context.Context, client *daemonclient.Client, event, property, value string) error {
	select {
	case g.lock <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-g.lock }()
	if err := ctx.Err(); err != nil {
		return err
	}
	now := g.now()
	day := now.UTC().Format(time.DateOnly)
	if g.state.Day != day {
		g.state = g.read(day)
		g.state.Day = day
		if g.state.Sent == nil {
			g.state.Sent = make(map[string]bool)
		}
	}
	key := event + ":" + value
	// Session ends and shown errors count every occurrence.
	daily := event != telemetry.EventSessionEnded && event != telemetry.EventErrorShown
	if g.state.Disabled || g.stopped || now.Before(g.retryAt) || daily && g.state.Sent[key] {
		return nil
	}

	receipt, err := sendActionTelemetry(ctx, client, "tui", event, property, value)
	if err == nil {
		switch receipt.Status {
		case apiclient.TelemetryEventResponseStatusDisabled:
			g.state.Disabled = true
		case apiclient.TelemetryEventResponseStatusQueued:
			if daily {
				g.state.Sent[key] = true
			}
		default:
			err = fmt.Errorf("telemetry receipt has unknown status %q", receipt.Status)
		}
	}
	if err != nil {
		return g.fail(err)
	}
	g.failures = 0
	g.policy.Reset()
	if daily || g.state.Disabled {
		return g.write()
	}
	return nil
}

// fail counts one failure in a row. A client error other than a
// timeout or rate limit stops the process at once.
func (g *tuiTelemetryGate) fail(err error) error {
	var status interface{ StatusCode() int }
	code := 0
	if errors.As(err, &status) {
		code = status.StatusCode()
	}
	g.failures++
	if code >= 400 && code < 500 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests || g.failures >= tuiTelemetryFailures {
		g.stopped = true
	} else {
		g.retryAt = g.now().Add(g.policy.NextBackOff())
	}
	return err
}

// read returns today's saved receipts. A missing, malformed, or older file is
// treated as fresh; the daemon still answers for the day.
func (g *tuiTelemetryGate) read(day string) tuiTelemetryState {
	var state tuiTelemetryState
	if g.path == "" {
		return state
	}
	data, err := os.ReadFile(g.path)
	if err != nil || json.Unmarshal(data, &state) != nil || state.Day != day {
		return tuiTelemetryState{}
	}
	return state
}

// write saves the in-memory receipts while lock is held, keeping writes in order.
// A completion from a previous UTC day leaves the current day's file alone.
func (g *tuiTelemetryGate) write() error {
	if g.path == "" || g.state.Day != g.now().UTC().Format(time.DateOnly) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(g.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(g.state)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(g.path, data, atomicfile.WithPerm(0o600))
}
