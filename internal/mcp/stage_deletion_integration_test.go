package mcp

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestStageDeletionThroughDaemon(t *testing.T) {
	checks := assert.New(t)
	setup := require.New(t)
	f := storetest.New(t)
	st := f.Store
	setup.NoError(st.AddAccountIdentity(f.Source.ID, "work@example.test", "manual"))
	persist := func(source int64, key, subject, delivered string) {
		conversation, err := st.EnsureConversation(source, key, "Synthetic selection")
		setup.NoError(err)
		header := ""
		if delivered != "" {
			header = "X-Delivered-To: " + delivered + "\r\n"
		}
		_, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{
			Message: &store.Message{
				SourceID: source, ConversationID: conversation, SourceMessageID: key,
				MessageType: "email", Subject: sql.NullString{String: subject, Valid: true},
			},
			BodyText: sql.NullString{String: "bodyneedle", Valid: true},
			FTS:      &store.FTSDoc{Subject: subject, Body: "bodyneedle"},
			RawMIME:  []byte(header + "Subject: " + subject + "\r\n\r\nbodyneedle"),
		})
		setup.NoError(err)
	}
	want := make([]string, 600)
	for i := range want {
		want[i] = fmt.Sprintf("selected-%03d", i)
		persist(f.Source.ID, want[i], "selectionmarker", "work@example.test")
	}
	persist(f.Source.ID, "unattributed", "selectionmarker", "")
	persist(f.Source.ID, "metadata-match", "bodyneedle", "")
	persist(f.Source.ID, "source-deleted", "selectionmarker", "work@example.test")
	setup.NoError(st.MarkMessageDeleted(f.Source.ID, "source-deleted"))
	other, err := st.GetOrCreateSource("gmail", "other@example.test")
	setup.NoError(err)
	persist(other.ID, "selected-000", "selectionmarker", "")
	nondeletable, err := st.GetOrCreateSource("slack", "workspace.example.test")
	setup.NoError(err)
	persist(nondeletable.ID, "nondeletable", "selectionmarker", "")
	// A separate unscoped marker proves source eligibility, rather than account filtering.
	persist(f.Source.ID, "eligible", "eligibilitymarker", "")
	persist(nondeletable.ID, "ineligible", "eligibilitymarker", "")
	history, err := st.GetOrCreateSource("gmail", "history@example.test")
	setup.NoError(err)
	persist(history.ID, "archive-only", "selectionmarker", "")
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: history.ID, IntoSourceID: f.Source.ID})
	setup.NoError(err)

	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	daemon := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: st, Engine: engine, Logger: slog.New(slog.DiscardHandler)}).Router())
	defer daemon.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, AllowInsecure: true, HTTPClient: daemon.Client()})
	setup.NoError(err)
	defer func() { checks.NoError(client.Close()) }()
	remote := daemonclient.NewEngineAdapter(client)
	// These are real capped HTTP searches: demonstrate the regression fixture exceeds both page limits.
	fast, err := remote.SearchFast(t.Context(), search.Parse("subject:selectionmarker"), query.MessageFilter{}, 100000, 0)
	setup.NoError(err)
	checks.Len(fast, 500)
	deep, err := remote.Search(t.Context(), search.Parse("bodyneedle"), 100000, 0)
	setup.NoError(err)
	checks.Len(deep, 100)
	for _, account := range []struct {
		name, key string
		want      []string
	}{
		{"physical", f.Source.Identifier, append(append([]string{}, want...), "unattributed")},
		{"attributed", store.VirtualIdentityKey(f.Source.ID, "work@example.test"), want},
		{"address", "work@example.test", want},
		{"unattributed", store.VirtualUnattributedKey(f.Source.ID), []string{"unattributed"}},
	} {
		for _, q := range []string{"subject:selectionmarker", "bodyneedle subject:selectionmarker | absentword subject:selectionmarker", "bodyneedle subject:selectionmarker"} {
			t.Run(account.name+"/"+q, func(t *testing.T) {
				checks := assert.New(t)
				setup := require.New(t)
				saver := &captureDeletionManifestSaver{}
				h := &handlers{engine: remote, manifestSaver: saver}
				resp := runTool[stageDeletionResponse](t, "stage_deletion", h.stageDeletion, map[string]any{"query": q, "account": account.key})
				setup.NotNil(saver.manifest)
				checks.ElementsMatch(account.want, saver.manifest.GmailIDs)
				checks.Equal(len(account.want), resp.MessageCount)
			})
		}
	}
	// Keep metadata-first semantics when other messages match only in the body.
	metadataSaver := &captureDeletionManifestSaver{}
	metadataHandler := &handlers{engine: remote, manifestSaver: metadataSaver}
	metadataResponse := runTool[stageDeletionResponse](t, "stage_deletion", metadataHandler.stageDeletion, map[string]any{"query": "bodyneedle", "account": f.Source.Identifier})
	setup.NotNil(metadataSaver.manifest)
	checks.Equal([]string{"metadata-match"}, metadataSaver.manifest.GmailIDs)
	checks.NotContains(metadataResponse.NextStep, "body matches")
	saver := &captureDeletionManifestSaver{}
	h := &handlers{engine: remote, manifestSaver: saver}
	runTool[stageDeletionResponse](t, "stage_deletion", h.stageDeletion, map[string]any{"query": "subject:eligibilitymarker"})
	setup.NotNil(saver.manifest)
	checks.Equal([]string{"eligible"}, saver.manifest.GmailIDs)
}
