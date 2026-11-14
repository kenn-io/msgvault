package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDiagnoseFTSOutputAndExit(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		for _, indexed := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{true: "json", false: "text"}[jsonOut], map[bool]string{true: "matched", false: "missing"}[indexed]}, "/"), func(t *testing.T) {
				a, r := assert.New(t), require.New(t)
				f := storetest.New(t)
				id := f.CreateMessage("private-synthetic-identifier")
				r.NoError(f.Store.UpsertMessageBodyContext(t.Context(), id, sql.NullString{String: "private-synthetic-body", Valid: true}, sql.NullString{}))
				if indexed {
					r.NoError(f.Store.IndexStoredMessageContext(t.Context(), id))
				}
				cmd := &cobra.Command{Use: "diagnose-fts", SilenceUsage: true, RunE: func(cmd *cobra.Command, _ []string) error {
					return runFTSDiagnostic(cmd, f.Store, store.FTSDiagnosticOptions{}, jsonOut)
				}}
				cmd.SetContext(t.Context())
				cmd.SetArgs([]string{})
				var output, stderr bytes.Buffer
				cmd.SetOut(&output)
				cmd.SetErr(&stderr)
				err := cmd.Execute()
				if indexed {
					r.NoError(err)
					a.Empty(stderr.String())
				} else {
					r.ErrorContains(err, "defects")
					if jsonOut {
						a.Contains(stderr.String(), "FTS diagnostic result: defects")
					} else {
						a.Empty(stderr.String(), "the text report already explains the failure")
					}
				}
				a.NotContains(output.String(), "private-synthetic")
				if jsonOut {
					var report store.FTSDiagnosticReport
					r.NoError(json.Unmarshal(output.Bytes(), &report))
					a.True(report.Complete)
					a.Equal(1, report.Checked)
					// Read the wire field explicitly: a missing numeric field would
					// otherwise also decode as zero and conceal SQLite's convention.
					var wire struct {
						Backend string `json:"backend"`
						Version *int   `json:"indexing_version"`
					}
					r.NoError(json.Unmarshal(output.Bytes(), &wire))
					r.NotNil(wire.Version)
					wantBackend, wantVersion := "sqlite", 0
					if f.Store.IsPostgreSQL() {
						wantBackend, wantVersion = "postgres", store.CurrentFTSIndexingVersion
					}
					a.Equal(wantBackend, wire.Backend)
					a.Equal(wantVersion, *wire.Version)
				} else if !indexed {
					a.Contains(output.String(), "search document absent")
				}
			})
		}
	}
}

func TestDiagnoseFTSInvalidFlags(t *testing.T) {
	for _, tc := range []struct {
		name, arg, want string
	}{
		{name: "limit below minimum", arg: "--limit=0", want: "--limit must be 1..1000 and --timeout must be 1s..5m"},
		{name: "limit above maximum", arg: "--limit=1001", want: "--limit must be 1..1000 and --timeout must be 1s..5m"},
		{name: "timeout below minimum", arg: "--timeout=0s", want: "--limit must be 1..1000 and --timeout must be 1s..5m"},
		{name: "timeout above maximum", arg: "--timeout=6m", want: "--limit must be 1..1000 and --timeout must be 1s..5m"},
		{name: "positional argument", arg: "unexpected", want: `unknown command "unexpected" for "diagnose-fts"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newDiagnoseFTSCmd()
			cmd.SetContext(t.Context())
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{tc.arg})
			require.ErrorContains(t, cmd.Execute(), tc.want)
		})
	}
}

type ftsDiagnosticFailWriter struct{}

func (ftsDiagnosticFailWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic output failure")
}

func TestDiagnoseFTSOutputFailure(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		require.ErrorContains(t, writeFTSDiagnostic(ftsDiagnosticFailWriter{}, store.FTSDiagnosticReport{}, jsonOut), "synthetic output failure")
	}
}

func TestDiagnoseFTSComparisonFailurePrintedOnce(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[jsonOut], func(t *testing.T) {
			a, r := assert.New(t), require.New(t)
			f := storetest.New(t)
			id := f.CreateMessage("synthetic-sql-failure")
			r.NoError(f.Store.IndexStoredMessageContext(t.Context(), id))
			statement := `DROP TABLE messages_fts`
			if f.Store.IsPostgreSQL() {
				statement = `ALTER TABLE messages DROP COLUMN search_fts_fields`
			}
			// Break only this fixture's search representation. The real Store
			// must return a failed check with a continuation value we can report.
			_, err := f.Store.DB().ExecContext(t.Context(), statement)
			r.NoError(err)
			cmd := &cobra.Command{Use: "diagnose-fts", SilenceUsage: true, RunE: func(cmd *cobra.Command, _ []string) error {
				return runFTSDiagnostic(cmd, f.Store, store.FTSDiagnosticOptions{}, jsonOut)
			}}
			cmd.SetContext(t.Context())
			cmd.SetArgs([]string{})
			var output, stderr bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&stderr)
			err = cmd.Execute()
			r.ErrorContains(err, "check failed; retry with --cursor")
			if jsonOut {
				var report store.FTSDiagnosticReport
				r.NoError(json.Unmarshal(output.Bytes(), &report))
				a.Equal("comparison_failed", report.Reason)
				a.Equal("incomplete", report.Outcome)
				a.False(report.Complete)
				a.NotEmpty(report.NextCursor)
				a.Contains(stderr.String(), "check failed; retry with --cursor")
			} else {
				a.Empty(stderr.String(), "the text report already explains why the check stopped")
				a.Equal(1, strings.Count(strings.ToLower(output.String()+stderr.String()), "check failed; retry with --cursor"))
				a.Equal(1, strings.Count(output.String(), "Continue: msgvault diagnose-fts --cursor "))
			}
		})
	}
}

func TestDiagnoseFTSReportErrorRouting(t *testing.T) {
	checkFailure := errors.New("message check failed; retry with --cursor and the reported continuation value")
	// Each case represents a different way the Store can finish. These tests
	// exercise the real renderer and Cobra, including failures with no reason
	// in the report: those still need stderr to explain what went wrong.
	for _, tc := range []struct {
		name        string
		report      store.FTSDiagnosticReport
		cause       error
		explanation string
	}{
		{name: "success", report: store.FTSDiagnosticReport{Outcome: "ok", Complete: true}},
		{name: "defect", report: store.FTSDiagnosticReport{Outcome: "defects", Complete: true, Checked: 1, Defects: 1, Entries: []store.FTSDiagnosticEntry{{MessageID: 1, State: "defect", Issues: []string{"missing"}}}}, explanation: "search document absent"},
		{name: "page limit", report: store.FTSDiagnosticReport{Outcome: "incomplete", Reason: "page_limit", Checked: 1, Matched: 1, NextCursor: "synthetic-cursor", Entries: []store.FTSDiagnosticEntry{{MessageID: 1, State: "matched", Issues: []string{}}}}, explanation: "more selected messages remain to be checked"},
		{name: "input limit", report: store.FTSDiagnosticReport{Outcome: "incomplete", Complete: true, Checked: 1, Inconclusive: 1, Entries: []store.FTSDiagnosticEntry{{MessageID: 1, State: "inconclusive", Issues: []string{}, Reason: "input_limit"}}}, explanation: "message text or recipient data exceeds the diagnostic limits; comparison could not be completed"},
		{name: "index unavailable", report: store.FTSDiagnosticReport{Outcome: "incomplete", Reason: "fts_unavailable", NextCursor: "synthetic-cursor"}, explanation: "search index is unavailable"},
		{name: "selection failed", report: store.FTSDiagnosticReport{Outcome: "incomplete", Reason: "selection_failed", NextCursor: "synthetic-cursor"}, cause: errors.New("FTS diagnostic selection failed"), explanation: "Message check failed; retry with --cursor and the reported continuation value"},
		{name: "comparison failed", report: store.FTSDiagnosticReport{Outcome: "incomplete", Reason: "comparison_failed", NextCursor: "synthetic-cursor"}, cause: checkFailure, explanation: "Message check failed; retry with --cursor and the reported continuation value"},
		{name: "canceled", report: store.FTSDiagnosticReport{Outcome: "incomplete", Reason: "canceled"}, cause: context.Canceled, explanation: "verification interrupted"},
		{name: "interrupted", report: store.FTSDiagnosticReport{Outcome: "incomplete", Reason: "interrupted"}, cause: context.Canceled, explanation: "verification interrupted"},
		{name: "timeout", report: store.FTSDiagnosticReport{Outcome: "incomplete", Reason: "timeout"}, cause: context.DeadlineExceeded, explanation: "verification reached its time limit"},
		{name: "startup error without reason", report: store.FTSDiagnosticReport{Outcome: "incomplete"}, cause: errors.New("invalid FTS diagnostic cursor")},
	} {
		for _, jsonOut := range []bool{false, true} {
			t.Run(tc.name+"/"+map[bool]string{false: "text", true: "json"}[jsonOut], func(t *testing.T) {
				a, r := assert.New(t), require.New(t)
				report := tc.report
				report.Backend = "sqlite"
				if report.Entries == nil {
					report.Entries = []store.FTSDiagnosticEntry{}
				}
				cmd := &cobra.Command{Use: "diagnose-fts", SilenceUsage: true, SilenceErrors: true, RunE: func(cmd *cobra.Command, _ []string) error {
					return reportFTSDiagnostic(cmd, report, tc.cause, jsonOut)
				}}
				cmd.SetContext(t.Context())
				cmd.SetArgs([]string{})
				var output, stderr bytes.Buffer
				cmd.SetOut(&output)
				cmd.SetErr(&stderr)
				err := cmd.Execute()
				switch {
				case tc.cause != nil:
					r.ErrorIs(err, tc.cause, "the underlying error still determines the exit status")
				case report.Outcome != "ok":
					r.ErrorContains(err, report.Outcome)
				default:
					r.NoError(err)
				}
				needsStderr := err != nil && (jsonOut || tc.cause != nil && report.Reason == "")
				if needsStderr {
					a.Equal(1, strings.Count(stderr.String(), err.Error()))
				} else {
					a.Empty(stderr.String())
				}
				if jsonOut {
					var got store.FTSDiagnosticReport
					r.NoError(json.Unmarshal(output.Bytes(), &got))
					a.Equal(report, got, "error routing must not change the JSON report")
				} else {
					a.Equal(1, strings.Count(output.String(), "FTS sqlite:"))
					if tc.explanation != "" {
						a.Equal(1, strings.Count(strings.ToLower(output.String()+stderr.String()), strings.ToLower(tc.explanation)))
					}
					if report.NextCursor != "" {
						a.Equal(1, strings.Count(output.String(), "Continue: msgvault diagnose-fts --cursor "+report.NextCursor))
					}
				}
			})
		}
	}
}

type ftsDiagnosticPartialWriter struct {
	buffer    bytes.Buffer
	remaining int
}

func (w *ftsDiagnosticPartialWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		n, _ := w.buffer.Write(p[:w.remaining])
		w.remaining = 0
		return n, errors.New("synthetic output failure")
	}
	w.remaining -= len(p)
	n, _ := w.buffer.Write(p) // bytes.Buffer.Write cannot fail.
	return n, nil
}

func TestDiagnoseFTSReportWriteErrorsRemainVisible(t *testing.T) {
	for _, jsonOut := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[jsonOut], func(t *testing.T) {
			a, r := assert.New(t), require.New(t)
			report := store.FTSDiagnosticReport{Backend: "sqlite", Outcome: "incomplete", Reason: "comparison_failed", NextCursor: "synthetic-cursor", Entries: []store.FTSDiagnosticEntry{}}
			var complete bytes.Buffer
			r.NoError(writeFTSDiagnostic(&complete, report, jsonOut))
			// Fail before any bytes, halfway through, or at the final byte.
			// Suppressing a reported check error must never hide a broken pipe
			// or claim that a partially written continuation value was delivered.
			for _, budget := range []int{0, complete.Len() / 2, complete.Len() - 1} {
				output := &ftsDiagnosticPartialWriter{remaining: budget}
				cmd := &cobra.Command{Use: "diagnose-fts", SilenceUsage: true, SilenceErrors: true, RunE: func(cmd *cobra.Command, _ []string) error {
					return reportFTSDiagnostic(cmd, report, errors.New("synthetic check failure"), jsonOut)
				}}
				cmd.SetContext(t.Context())
				cmd.SetArgs([]string{})
				var stderr bytes.Buffer
				cmd.SetOut(output)
				cmd.SetErr(&stderr)
				err := cmd.Execute()
				r.ErrorContains(err, "synthetic output failure")
				a.Equal(1, strings.Count(stderr.String(), "synthetic output failure"))
				a.NotContains(stderr.String(), "synthetic check failure")
			}
		})
	}
}
