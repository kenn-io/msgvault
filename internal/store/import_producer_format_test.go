package store_test

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestImportMessageCaptureUsesTrustedFormatAndIngestionTime(t *testing.T) {
	for _, notify := range []bool{false, true} {
		name := "silent"
		if notify {
			name = "notifying"
		}
		t.Run(name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			f := storetest.New(t)
			const owner = "owner:synthetic-import-producer"
			const family = "msgvault.import_message_archived"
			_, err := f.Store.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: owner, ImportNotifications: true, Capabilities: []store.MCPEventCapability{{Family: family, SourceType: "eml", Kinds: []string{"message"}}, {Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}}})
			require.NoError(err)
			execution, err := f.Store.AcquireSyncExecutionContext(t.Context(), f.Source.ID)
			require.NoError(err)
			t.Cleanup(func() { Assert.NoError(t, execution.Release()) })
			syncID, err := execution.StartSyncContext(t.Context(), "import-eml", "")
			require.NoError(err)
			scoped := f.Store.ScopedToSync(f.Source.ID, syncID)
			run, err := scoped.BeginImportAttempt(t.Context(), store.ImportAttemptRequest{Principal: owner, SourceID: f.Source.ID, Format: "eml", NotifyImports: notify, SyncRunID: syncID})
			require.NoError(err)
			view, err := scoped.WithImportRun(t.Context(), run.ID)
			require.NoError(err)
			sent := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
			message := f.NewMessage().WithSourceMessageID("synthetic-first-import").Build()
			message.SentAt = sql.NullTime{Time: sent, Valid: true}
			id, err := view.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: message, BodyText: sql.NullString{String: "Synthetic canonical body", Valid: true}, RawMIME: []byte("Subject: Synthetic imported snapshot\r\n\r\nSynthetic canonical body")})
			require.NoError(err)
			var liveCount, importCount int
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.message_archived'`).Scan(&liveCount))
			assert.Zero(liveCount, "a custom Gmail source label cannot change trusted offline provenance")
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*) FROM mcp_event_log WHERE family=?`), family).Scan(&importCount))
			if !notify {
				assert.Zero(importCount)
				return
			}
			require.Equal(1, importCount, "both run choice and actual EML capability authorize one first-import occurrence")
			var occurred, recorded, data string
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT occurred_at,recorded_at,data FROM mcp_event_log WHERE family=?`), family).Scan(&occurred, &recorded, &data))
			var payload map[string]any
			require.NoError(json.Unmarshal([]byte(data), &payload))
			assert.Equal("import", payload["ingestion_mode"])
			assert.Equal("eml", payload["import_format"])
			assert.Equal(run.ID, payload["import_run_id"])
			assert.Equal(strconv.FormatInt(id, 10), payload["message_id"])
			assert.Equal(sent.Format(time.RFC3339Nano), payload["sent_at"])
			timestamp, err := time.Parse(time.RFC3339Nano, occurred)
			require.NoError(err)
			archiveText, ok := payload["archived_at"].(string)
			require.True(ok)
			archived, err := time.Parse(time.RFC3339Nano, archiveText)
			require.NoError(err)
			assert.True(archived.Equal(timestamp), "event time is canonical ingestion time, independently of old sent time")
			assert.False(sent.Equal(timestamp))
			_, err = time.Parse(time.RFC3339Nano, recorded)
			require.NoError(err)
			assert.NotContains(data, "Synthetic canonical body")
		})
	}
}
