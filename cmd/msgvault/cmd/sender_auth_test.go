package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSenderAuthDiagnosticOutput(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	for _, jsonOutput := range []bool{false, true} {
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		report := &query.SenderAuthDiagnostics{SourceID: 7, Scanned: 1, SampleLimit: 1, HasMore: true, BasisCounts: map[string]int{"no_raw": 1}, Observations: []query.AuthservObservation{{AuthservID: "bad\x1b[2J\nreceiver.test", Count: 1, ContentTrust: "untrusted_text"}}}
		must.NoError(writeSenderAuthDiagnostics(cmd, report, jsonOutput, true))
		checks.NotContains(out.String(), "\x1b")
		checks.NotContains(out.String(), "bad\n")
		checks.Contains(out.String(), "receiver.test")
	}
	cmd := newSenderAuthCmd()
	cmd.SetArgs([]string{"status", "--source-id", "0"})
	must.ErrorContains(cmd.Execute(), "use a positive --source-id")
}

func TestSenderAuthCommandsThroughDaemon(t *testing.T) {
	checks := assert.New(t)
	must := require.New(t)
	f := storetest.New(t)
	pid := f.EnsureParticipant("sender@example.test", "Sender", "example.test")
	for range 2 {
		id := f.NewMessage().WithSubject("PRIVATE SUBJECT").Create(t, f.Store)
		must.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{pid}, nil))
		must.NoError(f.Store.UpsertMessageRaw(id, []byte("From: sender@example.test\r\nAuthentication-Results: mx.test; dmarc=pass header.from=example.test\r\n\r\nPRIVATE BODY")))
	}
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner"}, SenderAuth: config.SenderAuthConfig{Sources: []config.SenderAuthSource{{SourceID: f.Source.ID, TrustedAuthservIDs: []string{"mx.test"}}}}}
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: f.Store, config: cfg}, Engine: query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL()), Logger: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	server := httptest.NewServer(srv.Router())
	t.Cleanup(server.Close)
	remote := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Remote: config.RemoteConfig{APIKey: "owner"}}
	ctx := configureRemoteDaemonForTest(t, server.URL, remote)
	for _, mode := range []string{"status", "suggest"} {
		var output bytes.Buffer
		cmd := newSenderAuthCmd()
		cmd.SetContext(ctx)
		cmd.SetOut(&output)
		cmd.SetArgs([]string{mode, "--source-id", strconv.FormatInt(f.Source.ID, 10), "--limit", "1", "--json"})
		must.NoError(cmd.Execute())
		var report query.SenderAuthDiagnostics
		must.NoError(json.Unmarshal(output.Bytes(), &report))
		checks.Equal(1, report.Scanned)
		checks.True(report.HasMore)
		checks.Equal(1, report.Verified)
		checks.NotContains(output.String(), "PRIVATE SUBJECT")
		checks.NotContains(output.String(), "PRIVATE BODY")
		checks.NotContains(output.String(), "sender@example.test")
		if mode == "suggest" {
			must.Len(report.Observations, 1)
			checks.Equal("mx.test", report.Observations[0].AuthservID)
		} else {
			checks.Empty(report.Observations)
		}
	}
}
