package cmd

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/kataissues"
	"go.kenn.io/msgvault/internal/testutil/katatest"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestAgreementCLIResumesLastAcceptedTermsThroughDaemonAndKata(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	f := storetest.New(t)
	kataServer := httptest.NewServer(katatest.New(t).Service.Handler())
	t.Cleanup(kataServer.Close)
	cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}
	cfg.Integrations.Kata = config.TaskIntegrationConfig{Enabled: true, Endpoint: kataServer.URL, APIKey: katatest.Token, DefaultProject: "example"}
	daemon := httptest.NewServer(api.NewServer(cfg, &storeAPIAdapter{store: f.Store}, nil, slog.New(slog.DiscardHandler)).Router())
	t.Cleanup(daemon.Close)
	ctx := configureRemoteDaemonForTest(t, daemon.URL)
	var issue string
	terms := map[string]any{"text": "Send the budget", "due": "2026-11-06", "obligor": "Alex", "beneficiary": "Sam"}
	steps := []struct {
		id, action, actor, target, quote string
		terms                            bool
	}{
		{"r", "request", "beneficiary", "", "Please send the budget Friday.", true},
		{"a", "accept", "obligor", "r", "I will send it Friday.", false},
		{"p", "propose", "obligor", "a", "Could I send it Monday instead?", true},
		{"x", "replace", "beneficiary", "p", "Monday is agreed.", false},
	}
	var response struct {
		Agreement kataissues.AgreementView `json:"agreement"`
	}
	for count, step := range steps {
		message := f.CreateMessage("agreement-cli-" + step.id)
		_, err := f.Store.DB().Exec(f.Store.Rebind("INSERT INTO message_bodies(message_id,body_text) VALUES (?,?)"), message, step.quote)
		must.NoError(err)
		var prepared generated.KataEvidencePrepareResponse
		must.NoError(json.Unmarshal(runKataCommand(ctx, t, fmt.Sprintf(`{"selectors":[{"kind":"message","message_id":%d,"max_chars":1000}]}`, message), "evidence", "prepare"), &prepared))
		evidence := []generated.Reference{prepared.Evidence[0].Reference}
		if count == 0 {
			body, err := json.Marshal(generated.KataIssueCreateRequest{Title: "Budget agreement", Evidence: evidence})
			must.NoError(err)
			var receipt generated.KataIssueResponse
			must.NoError(json.Unmarshal(runKataCommand(ctx, t, string(body), "create", "--idempotency-key", "budget-agreement", "--json"), &receipt))
			issue = receipt.Issue.QualifiedRef
		} else {
			body, err := json.Marshal(generated.KataEvidenceLinkRequest{Evidence: evidence})
			must.NoError(err)
			runKataCommand(ctx, t, string(body), "link", issue, "--json")
		}
		input := map[string]any{"id": step.id, "action": step.action, "actor": step.actor, "target": step.target, "expected_events": count, "reviewed": true, "evidence": evidence}
		if step.terms {
			if count == 2 {
				terms["due"] = "2026-11-09"
			}
			input["terms"] = terms
		}
		body, err := json.Marshal(input)
		must.NoError(err)
		must.NoError(json.Unmarshal(runKataCommand(ctx, t, string(body), "agreement", "record", issue, "--json"), &response))
		if count == 0 {
			check.Nil(response.Agreement.Accepted)
		}
		if count == 2 {
			must.NotNil(response.Agreement.Accepted)
			check.Equal("2026-11-06", response.Agreement.Accepted.Due)
			daemon.Close()
			daemon = httptest.NewServer(api.NewServer(cfg, &storeAPIAdapter{store: f.Store}, nil, slog.New(slog.DiscardHandler)).Router())
			t.Cleanup(daemon.Close)
			ctx = configureRemoteDaemonForTest(t, daemon.URL)
			must.NoError(json.Unmarshal(runKataCommand(ctx, t, "", "agreement", "show", issue, "--json"), &response))
			must.NotNil(response.Agreement.Accepted)
			check.Equal("2026-11-06", response.Agreement.Accepted.Due)
		}
	}
	must.NotNil(response.Agreement.Accepted)
	check.Equal("2026-11-09", response.Agreement.Accepted.Due)
	must.Len(response.Agreement.History, 4)
	check.Equal("2026-11-06", response.Agreement.History[0].Input.Terms.Due)
	check.Equal("Monday is agreed.", response.Agreement.History[3].Quotes[0])
}

func TestAgreementCLIRejectsUnknownInputBeforeTransport(t *testing.T) {
	must := require.New(t)

	command := newKataCmd()
	command.SetArgs([]string{"agreement", "record", "example#abcd"})
	command.SetIn(strings.NewReader(`{"unknown":true}`))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	must.ErrorContains(command.ExecuteContext(t.Context()), "Kata input")
}

func TestAgreementCLIRefusesOlderDaemon(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Equal("/api/v1/health", r.URL.Path, "an older daemon must never receive an agreement call")
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"status":"ok","api_schema_version":"3.44.0"}`))
		check.NoError(err)
	}))
	t.Cleanup(daemon.Close)
	ctx := configureRemoteDaemonForTest(t, daemon.URL)
	input := `{"id":"r","action":"request","actor":"beneficiary","expected_events":0,"reviewed":true,"terms":{"text":"Deliver","obligor":"Alex","beneficiary":"Sam"},"evidence":[{"version":1,"kind":"message","archive_uid":"archive","message_id":1,"source_type":"email","source_identifier":"owner@example.com","source_message_id":"m1","message":{"body_sha256":"` + strings.Repeat("a", 64) + `","start_rune":0,"end_rune":1}}]}`
	for _, action := range []string{"show", "record"} {
		command := newKataCmd()
		command.SetArgs([]string{"agreement", action, "example#abcd", "--json"})
		command.SetIn(strings.NewReader(input))
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		must.ErrorContains(command.ExecuteContext(ctx), "3.45.0")
	}
}
