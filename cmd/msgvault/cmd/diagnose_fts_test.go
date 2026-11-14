package cmd

import (
	"bytes"
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
