package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
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
				cmd := &cobra.Command{}
				cmd.SetContext(t.Context())
				var output bytes.Buffer
				cmd.SetOut(&output)
				err := runFTSDiagnostic(cmd, f.Store, store.FTSDiagnosticOptions{}, jsonOut)
				if indexed {
					r.NoError(err)
				} else {
					r.ErrorContains(err, "defects")
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
	for _, args := range [][]string{{"--limit=0"}, {"--limit=1001"}, {"--timeout=0s"}, {"--timeout=6m"}, {"unexpected"}} {
		cmd := newDiagnoseFTSCmd()
		cmd.SetContext(t.Context())
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(args)
		require.Error(t, cmd.Execute())
	}
}

func TestDiagnoseFTSOlderDaemonRejectsCommand(t *testing.T) {
	// Execute the existing transport against the old admission response. The
	// HTTP contract matters here: an equal minor from another feature is not
	// evidence that this command exists.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, err := io.WriteString(w, `{"error":"command_not_allowed","message":"command is not allowed through the daemon CLI runner"}`)
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "synthetic-key", AllowInsecure: true})
	require.NoError(t, err)
	err = client.RunCLICommand(t.Context(), daemonclient.CLIRunRequest{Args: []string{"diagnose-fts", "--json"}}, nil)
	require.ErrorContains(t, err, "command is not allowed")
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
