package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/muesli"
)

func TestMuesliWatchSchedulesEnabledSourcesAndRescans(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	sources := []config.MuesliSource{{Identifier: "enabled", Enabled: true, Schedule: "* * * * *"}, {Identifier: "disabled", Enabled: false, Schedule: "* * * * *"}}
	selected, err := muesliWatchSources(sources)
	require.NoError(err)
	require.Len(selected, 1)
	assert.Equal("enabled", selected[0].source.Identifier)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	// A clock advances to each real cron transition; no throughput assumption.
	err = runMuesliWatch(ctx, selected, func(_ context.Context, _ config.MuesliSource) error {
		calls++
		if calls == 2 {
			cancel()
		}
		return errors.New("synthetic temporary transport failure")
	}, func(context.Context, time.Time) error { return nil }, func(error) {})
	require.ErrorIs(err, context.Canceled)
	assert.Equal(2, calls)
	for _, sources := range [][]config.MuesliSource{nil, {{Identifier: "no-schedule", Enabled: true}}, {{Identifier: "bad", Enabled: true, Schedule: "invalid"}}} {
		_, err := muesliWatchSources(sources)
		require.Error(err)
	}
}

func TestMuesliWatchRejectsPartialOrForcedScans(t *testing.T) {
	oldMeetingID := syncMuesliMeetingID
	oldWatch, oldLimit, oldAfter, oldFull := syncMuesliWatch, syncMuesliLimit, syncMuesliAfter, syncMuesliFull
	t.Cleanup(func() {
		syncMuesliMeetingID = oldMeetingID
		syncMuesliWatch, syncMuesliLimit, syncMuesliAfter, syncMuesliFull = oldWatch, oldLimit, oldAfter, oldFull
	})
	cfg := config.NewDefaultConfig()
	cfg.Remote.URL = "https://archive.example.com"
	off := false
	cfg.Muesli = []config.MuesliSource{{DBPath: filepath.Join(t.TempDir(), "muesli.db"), Contacts: &off, Identifier: "recorder", AccountEmail: "user@example.com", Enabled: true, Schedule: "* * * * *"}}
	for _, flag := range []string{"limit", "after", "full", "build-cache", "meeting-id"} {
		t.Run(flag, func(t *testing.T) {
			syncMuesliMeetingID = 0
			syncMuesliWatch = true
			syncMuesliLimit = 0
			syncMuesliAfter = ""
			syncMuesliFull = false
			command := addManualSyncCacheFlags(&cobra.Command{Use: "sync-muesli"})
			command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
			switch flag {
			case "meeting-id":
				syncMuesliMeetingID = 42
			case "limit":
				syncMuesliLimit = 1
			case "after":
				syncMuesliAfter = "2026-09-01"
			case "full":
				syncMuesliFull = true
			case "build-cache":
				require.NoError(t, command.Flags().Set("build-cache", "true"))
			}
			err := syncMuesliCmd.RunE(command, nil)
			require.ErrorContains(t, err, "--watch cannot combine")
		})
	}
}

func TestMuesliWatchRejectsImpossibleDate(t *testing.T) {
	_, err := muesliWatchSources([]config.MuesliSource{{Identifier: "recorder", Enabled: true, Schedule: "0 0 31 2 *"}})
	require.ErrorContains(t, err, "future transition")
}

func TestMuesliWatchFailureReasonsDoNotEchoValues(t *testing.T) {
	const privateValue = "synthetic-contact@example.com"
	for _, tc := range []struct {
		name string
		err  error
		hint string
	}{
		{"credentials", &daemonclient.APIError{Status: http.StatusUnauthorized, Message: privateValue}, "credentials"},
		{"registration", &daemonclient.APIError{Status: http.StatusNotFound, Code: "source_not_found", Message: privateValue}, "add-muesli"},
		{"version", &daemonclient.APIError{Status: http.StatusNotFound, Message: privateValue}, "endpoint"},
		{"rejection", &daemonclient.APIError{Status: http.StatusUnprocessableEntity, Message: privateValue}, "registration"},
		{"validation", fmt.Errorf("%s: %w", privateValue, muesli.ErrRemoteValidation), "validation"},
		{"size", fmt.Errorf("%s: %w", privateValue, muesli.ErrRemoteTooLarge), "16 MiB"},
		{"mixed", errors.Join(muesli.ErrRemoteValidation, &daemonclient.APIError{Status: http.StatusUnauthorized, Message: privateValue}), "credentials"},
		{"local", errors.New(privateValue), "local files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason := muesliWatchFailureReason(tc.err)
			assert.Contains(t, reason, tc.hint)
			assert.NotContains(t, reason, privateValue)
		})
	}
}

func TestMuesliWatchBusyMessageDoesNotEchoDaemonValues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	oldWatch, oldID := syncMuesliWatch, syncMuesliMeetingID
	t.Cleanup(func() { syncMuesliWatch, syncMuesliMeetingID = oldWatch, oldID })
	syncMuesliWatch, syncMuesliMeetingID = true, 0
	path := filepath.Join(t.TempDir(), "muesli.db")
	db, err := sql.Open("sqlite3", path)
	require.NoError(err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(`CREATE TABLE meetings (id INTEGER PRIMARY KEY,title TEXT,start_time TEXT,created_at TEXT,raw_transcript TEXT); INSERT INTO meetings VALUES(42,'Planning','2026-09-01T14:00:00Z','2026-09-01 14:00:03','Synthetic transcript')`)
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var requests atomic.Int32
	// Exercise the daemon's existing operation_in_progress HTTP contract.
	// A scheduled-sync gate label can contain account identity values.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/import/muesli", r.URL.Path)
		assert.Equal("synthetic-owner-key", r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"operation_in_progress","message":"scheduled sync of synthetic-private@example.com"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"unchanged","source_id":1}`))
		cancel()
	}))
	defer server.Close()
	cfg := config.NewDefaultConfig()
	cfg.Remote = config.RemoteConfig{URL: server.URL, APIKey: "synthetic-owner-key", AllowInsecure: true}
	off := false
	cfg.Muesli = []config.MuesliSource{{Identifier: "recorder", AccountEmail: "user@example.com", DBPath: path, Contacts: &off, Enabled: true, Schedule: "* * * * *"}}
	command := &cobra.Command{Use: "sync-muesli"}
	command.SetContext(testInvocationContext(ctx, cfg, invocationOptions{}))
	command.SetOut(io.Discard)
	var diagnostics bytes.Buffer
	command.SetErr(&diagnostics)
	err = syncMuesliCmd.RunE(command, []string{"recorder"})
	require.ErrorIs(err, context.Canceled)
	assert.Equal(int32(2), requests.Load())
	assert.Contains(diagnostics.String(), "daemon is busy; waiting to retry")
	assert.NotContains(diagnostics.String(), "synthetic-private@example.com")
}

func TestMuesliRemoteRejectsNegativeMeetingSelector(t *testing.T) {
	oldWatch, oldID := syncMuesliWatch, syncMuesliMeetingID
	t.Cleanup(func() { syncMuesliWatch, syncMuesliMeetingID = oldWatch, oldID })
	syncMuesliWatch, syncMuesliMeetingID = true, -1
	cfg := config.NewDefaultConfig()
	cfg.Remote.URL = "https://archive.example.com"
	off := false
	cfg.Muesli = []config.MuesliSource{{Identifier: "recorder", AccountEmail: "user@example.com", DBPath: filepath.Join(t.TempDir(), "missing.db"), Contacts: &off, Enabled: true, Schedule: "* * * * *"}}
	command := &cobra.Command{Use: "sync-muesli"}
	command.SetContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	err := syncMuesliCmd.RunE(command, []string{"recorder"})
	require.ErrorContains(t, err, "--meeting-id must be positive")
}

func TestMuesliWatchCancellationDoesNotReportFailure(t *testing.T) {
	for _, cancelOnScan := range []int{1, 2} {
		t.Run(fmt.Sprintf("scan%d", cancelOnScan), func(t *testing.T) {
			require := require.New(t)
			sources, err := muesliWatchSources([]config.MuesliSource{{Identifier: "recorder", Enabled: true, Schedule: "* * * * *"}})
			require.NoError(err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			scans, reports := 0, 0
			err = runMuesliWatch(ctx, sources, func(context.Context, config.MuesliSource) error {
				scans++
				if scans == cancelOnScan {
					cancel()
					return context.Canceled
				}
				return nil
			}, func(context.Context, time.Time) error { return nil }, func(error) { reports++ })
			require.ErrorIs(err, context.Canceled)
			assert.Zero(t, reports, "deliberate cancellation must not claim a future retry")
		})
	}
}
