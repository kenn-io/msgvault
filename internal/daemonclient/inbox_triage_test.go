package daemonclient_test

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

type inboxTriageClient interface {
	PreviewInboxTriage(ctx context.Context, input inboxcontrol.TriageInput) (*inboxcontrol.TriageProposal, error)
	ApplyInboxTriage(ctx context.Context, proposal inboxcontrol.TriageProposal) ([]inboxcontrol.Result, error)
}

func triageClientInput() (inboxcontrol.TriageInput, inboxcontrol.TriageProposal) {
	source := inboxcontrol.SourceIdentity{SourceID: 1, SourceType: "gmail", SourceIdentifier: "owner@example.test", AccountID: "owner@example.test"}
	input := inboxcontrol.TriageInput{Source: source}
	proposal := inboxcontrol.TriageProposal{Source: source, MappingRevision: 1, ArchiveRevision: strings.Repeat("a", 64), IncomingWatermark: strings.Repeat("b", 64), IssuedAt: time.Now().UTC(), PreviewToken: "opaque-caller-bound-token"}
	proposal.ExpiresAt = proposal.IssuedAt.Add(5 * time.Minute)
	for _, id := range []int64{7, 8} {
		target := inboxcontrol.Target{SourceID: 1, SourceType: "gmail", SourceIdentifier: source.SourceIdentifier, AccountID: source.AccountID, Scope: inboxcontrol.ScopeMessage, ItemID: id, ProviderID: string(rune('a' + id))}
		before := inboxcontrol.State{Target: target, Inbox: new(true), Read: new(false), Tags: []string{"INBOX", "UNREAD"}, Revision: "7", ObservedAt: proposal.IssuedAt}
		projected := before
		projected.Tags = []string{"INBOX", "UNREAD", "TodoLabel"}
		key := "item-" + target.ProviderID
		input.Items = append(input.Items, inboxcontrol.TriageItemInput{Target: target, Categories: []string{"todo"}, EvidenceMessageIDs: []int64{id}, IdempotencyKey: key})
		proposal.Items = append(proposal.Items, inboxcontrol.TriageProposalItem{Categories: []string{"todo"}, EvidenceMessageIDs: []int64{id}, Classification: "todo", RetainInbox: true, Request: inboxcontrol.Request{Operation: inboxcontrol.OpTags, Target: &target, Expected: &before, IdempotencyKey: key, Tags: &emailtags.Change{Add: []string{"TodoLabel"}}}, Projected: projected})
	}
	return input, proposal
}

func triageClientDiscovery() apiprotocol.MCPCapabilities {
	return apiprotocol.MCPCapabilities{Version: 1, Routes: []apiprotocol.MCPRouteDescriptor{
		{OperationID: "previewInboxTriage", Method: "POST", Path: "/api/v1/inbox/triage/preview", RequestProperties: []string{"source", "items"}},
		{OperationID: "applyInboxTriage", Method: "POST", Path: "/api/v1/inbox/triage/apply", RequestProperties: []string{"source", "items", "mapping_revision", "archive_revision", "incoming_watermark", "issued_at", "expires_at", "preview_token"}},
	}}
}

// This external HTTP boundary exercises the real generated request transport.
// Removing whole-envelope forwarding, current discovery, response validation,
// partial receipts, or redirect/retry protection must break these observations.
// Native cryptographic authority and provider writes are covered by Service/API.
func TestInboxTriageClientForwardsWholeProposalAndPreservesReceipts(t *testing.T) {
	for _, mode := range []string{"success", "partial", "unknown", "lost", "redirect", "empty", "short", "wrong-order", "missing-discovery", "partial-discovery", "wrong-method", "empty-evidence", "expired-replay"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			input, proposal := triageClientInput()
			if mode == "empty-evidence" {
				input.Items[0].EvidenceMessageIDs = []int64{}
				proposal.Items[0].EvidenceMessageIDs = []int64{}
			}
			var previews, applies, followed atomic.Int64
			destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Add(1) }))
			defer destination.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "synthetic-agent-token", r.Header.Get(apiprotocol.AgentTokenHeader))
				switch r.URL.Path {
				case "/api/v1/health":
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.3.0"}`))
				case "/api/v1/mcp/capabilities":
					d := triageClientDiscovery()
					if mode == "missing-discovery" {
						d.Routes = nil
					}
					if mode == "partial-discovery" {
						d.Routes[0].RequestProperties = []string{"source"}
					}
					if mode == "wrong-method" {
						d.Routes[0].Method = "GET"
					}
					assert.NoError(t, json.MarshalWrite(w, d))
				case "/api/v1/inbox/triage/preview":
					previews.Add(1)
					assert.Equal(t, http.MethodPost, r.Method)
					var got inboxcontrol.TriageInput
					if !assert.NoError(t, json.UnmarshalRead(r.Body, &got)) {
						return
					}
					assert.Equal(t, input, got)
					assert.NoError(t, json.MarshalWrite(w, proposal))
				case "/api/v1/inbox/triage/apply":
					applies.Add(1)
					assert.Equal(t, http.MethodPost, r.Method)
					var got inboxcontrol.TriageProposal
					if !assert.NoError(t, json.UnmarshalRead(r.Body, &got)) {
						return
					}
					assert.Equal(t, proposal, got)
					results := make([]inboxcontrol.Result, 2)
					for i, id := range []string{"receipt-one", "receipt-two"} {
						item := proposal.Items[i]
						intent := item.Request
						intent.DryRun, intent.Expected, intent.IdempotencyKey = true, nil, ""
						results[i] = inboxcontrol.Result{Before: item.Request.Expected, Projected: &item.Projected, After: &item.Projected, Receipt: &inboxcontrol.Receipt{ID: id, PrincipalID: "synthetic-principal", SourceID: 1, IdempotencyKey: item.Request.IdempotencyKey, IntentHash: strings.Repeat("c", 64), StateHash: strings.Repeat("d", 64), Intent: intent, Before: *item.Request.Expected, Projected: &item.Projected, After: &item.Projected, Status: inboxcontrol.StatusVerified, CreatedAt: proposal.IssuedAt}}
					}
					switch mode {
					case "partial", "unknown":
						code := "inbox_conflict"
						status := http.StatusConflict
						results[1] = inboxcontrol.Result{}
						if mode == "unknown" {
							code = "inbox_outcome_unknown"
							status = http.StatusBadGateway
							results[0].Receipt.Status = inboxcontrol.StatusUnknown
						}
						w.WriteHeader(status)
						assert.NoError(t, json.MarshalWrite(w, map[string]any{"error": code, "message": "untrusted provider detail", "results": results}))
					case "lost":
						hijacker, ok := w.(http.Hijacker)
						if !assert.True(t, ok) {
							return
						}
						conn, _, err := hijacker.Hijack()
						if !assert.NoError(t, err) {
							return
						}
						assert.NoError(t, conn.Close())
					case "redirect":
						http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
					case "empty":
						_, _ = w.Write([]byte(`[]`))
					case "short":
						assert.NoError(t, json.MarshalWrite(w, results[:1]))
					case "wrong-order":
						assert.NoError(t, json.MarshalWrite(w, []inboxcontrol.Result{results[1], results[0]}))
					default:
						assert.NoError(t, json.MarshalWrite(w, results))
					}
				default:
					assert.Fail(t, "unexpected triage client request", r.URL.Path)
				}
			}))
			defer server.Close()
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, AgentToken: "synthetic-agent-token", HTTPClient: server.Client()})
			requirements.NoError(err)
			defer func() { assert.NoError(t, client.Close()) }()
			triage, ok := any(client).(inboxTriageClient)
			requirements.True(ok, "daemon client must expose bounded triage preview/apply")
			got, err := triage.PreviewInboxTriage(t.Context(), input)
			if mode == "missing-discovery" || mode == "partial-discovery" || mode == "wrong-method" {
				require.ErrorIs(t, err, inboxcontrol.ErrUnavailable)
				assertions.Nil(got)
				assertions.Zero(previews.Load())
				return
			}
			requirements.NoError(err)
			requirements.NotNil(got)
			assertions.Equal(proposal, *got)
			if mode == "expired-replay" {
				proposal.IssuedAt = proposal.IssuedAt.Add(-10 * time.Minute)
				proposal.ExpiresAt = proposal.ExpiresAt.Add(-10 * time.Minute)
				*got = proposal
			}
			results, err := triage.ApplyInboxTriage(t.Context(), *got)
			switch mode {
			case "success", "empty-evidence", "expired-replay":
				requirements.NoError(err)
				requirements.Len(results, 2)
				assertions.Equal("receipt-one", results[0].Receipt.ID)
			case "partial", "unknown":
				want := inboxcontrol.ErrConflict
				if mode == "unknown" {
					want = inboxcontrol.ErrOutcomeUnknown
				}
				require.ErrorIs(t, err, want)
				requirements.Len(results, 2)
				requirements.NotNil(results[0].Receipt)
				assertions.Equal("receipt-one", results[0].Receipt.ID)
				assertions.Nil(results[1].Receipt)
				assertions.NotContains(err.Error(), "untrusted provider detail")
			default:
				require.ErrorIs(t, err, inboxcontrol.ErrOutcomeUnknown)
			}
			assertions.Equal(int64(1), previews.Load())
			assertions.Equal(int64(1), applies.Load())
			assertions.Zero(followed.Load())
		})
	}
}

func TestInboxTriageClientRejectsInvalidBeforeHTTP(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	requirements.NoError(err)
	defer func() { assert.NoError(t, client.Close()) }()
	triage, ok := any(client).(inboxTriageClient)
	requirements.True(ok)
	for _, count := range []int{0, 101} {
		input, _ := triageClientInput()
		input.Items = make([]inboxcontrol.TriageItemInput, count)
		_, err := triage.PreviewInboxTriage(t.Context(), input)
		requirements.ErrorIs(err, inboxcontrol.ErrInvalid)
	}
	for _, mode := range []string{"unsigned", "remove", "inner-token", "foreign-source", "empty-key"} {
		_, proposal := triageClientInput()
		switch mode {
		case "unsigned":
			proposal.PreviewToken = ""
		case "remove":
			proposal.Items[0].Request.Tags.Remove = []string{"INBOX"}
		case "inner-token":
			proposal.Items[0].Request.PreviewToken = "standalone-token"
		case "foreign-source":
			proposal.Source.AccountID = "other@example.test"
		case "empty-key":
			proposal.Items[0].Request.IdempotencyKey = ""
		}
		_, err := triage.ApplyInboxTriage(t.Context(), proposal)
		require.Error(t, err)
	}
	assertions.Zero(calls.Load())
}
