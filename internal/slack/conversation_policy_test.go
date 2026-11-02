package slack

import (
	"encoding/json"
	"fmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func coldPolicyTier(t *testing.T, st *store.Store, providerID string) conversationpolicy.Tier {
	t.Helper()
	rows, err := st.ListConversationPolicies(t.Context())
	require.NoError(t, err)
	for _, row := range rows {
		if row.ProviderID == providerID {
			return row.EffectiveTier
		}
	}
	require.FailNow(t, "missing policy", providerID)
	return ""
}

func TestImporterColdPolicyRetainsTextAndMediaPolicy(t *testing.T) {
	for _, tc := range []struct {
		name              string
		count             int
		unknown, disabled bool
		override          conversationpolicy.Tier
		want              conversationpolicy.Tier
	}{
		{name: "twenty", count: 20, want: conversationpolicy.Full}, {name: "twenty-one", count: 21, want: conversationpolicy.Cold},
		{name: "unknown", unknown: true, want: conversationpolicy.Cold}, {name: "disabled", count: 21, disabled: true, want: conversationpolicy.Full},
		{name: "explicit-full", count: 21, override: conversationpolicy.Full, want: conversationpolicy.Full}, {name: "explicit-cold", count: 20, override: conversationpolicy.Cold, want: conversationpolicy.Cold},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			f := testWorkspace(t)
			f.convs = f.convs[:1]
			c := f.convs[0]
			c.Msgs = []fakeMsg{{TS: ts(0), User: "UALICE", Text: "retained text", Files: []map[string]any{{"id": "FILE", "name": "synthetic.png", "mimetype": "image/png", "size": 12, "url_private": "https://files.slack.com/synthetic"}}}}
			c.Members = nil
			for i := range tc.count {
				c.Members = append(c.Members, fmt.Sprintf("U%02d", i))
			}
			f.failMembers[c.ID] = tc.unknown
			imp, opts := testImporter(t, f)
			opts.NoMedia = false
			opts.AttachmentsDir = t.TempDir()
			opts.MediaPolicy = attachmentpolicy.Policy{MaxParticipants: 1}
			settings := conversationpolicy.RecommendedSettings()
			if tc.disabled {
				settings.MaxParticipants["slack"] = 0
				settings.MaxParticipants["slackdump"] = 0
			}
			if tc.override != "" {
				settings.Overrides = []conversationpolicy.Override{{SourceType: "slack", SourceIdentifier: "T01:UME", ConversationID: c.ID, Tier: tc.override}}
			}
			_, err := imp.store.ConfigureConversationPolicy(t.Context(), settings, time.Now())
			requirements.NoError(err)
			sum, err := imp.Import(t.Context(), opts)
			if tc.unknown {
				requirements.Error(err)
			} else {
				requirements.NoError(err)
			}
			requirements.NotNil(sum)
			var count int
			requirements.NoError(imp.store.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
			assertions.Equal(1, count)
			assertions.Equal(tc.want, coldPolicyTier(t, imp.store, c.ID))
			assertions.Zero(sum.AttachmentsDownloaded)
			assertions.Equal(1, sum.AttachmentsSkipped)
			var mediaReason string
			requirements.NoError(imp.store.DB().QueryRow(`SELECT attachment_skip_reason FROM attachments`).Scan(&mediaReason))
			assertions.Equal(string(attachmentpolicy.SkipParticipantThreshold), mediaReason)
			message, err := imp.store.InspectMessage(sourceMessageID(c.ID, ts(0)))
			requirements.NoError(err)
			assertions.Equal("retained text", message.BodyText)
			if !tc.unknown {
				state, err := imp.loadResumeState(t.Context(), sum.SourceID)
				requirements.NoError(err)
				assertions.True(state.Conversations[c.ID].Done)
				assertions.NotEmpty(state.Conversations[c.ID].Cursor)
			}
		})
	}
}
func TestImporterColdPolicySameSyncActivityPromotion(t *testing.T) {
	for _, signal := range []string{"own", "mention", "reply"} {
		t.Run(signal, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			f := testWorkspace(t)
			f.convs = f.convs[:1]
			c := f.convs[0]
			c.Members = nil
			for i := range 21 {
				c.Members = append(c.Members, fmt.Sprintf("U%02d", i))
			}
			old := fmt.Sprintf("%d.000100", time.Now().Add(-181*24*time.Hour).Unix())
			c.Msgs = []fakeMsg{{TS: ts(0), User: "UALICE", Text: "quiet"}}
			switch signal {
			case "own":
				c.Msgs[0].User = "UME"
			case "mention":
				c.Msgs[0].Text = "hi <@UME>"
			case "reply":
				c.Msgs = []fakeMsg{{TS: old, User: "UME", Text: "old own", Replies: []fakeMsg{{TS: ts(0), ThreadTS: old, User: "UALICE", Text: "direct reply"}}}}
			}
			imp, opts := testImporter(t, f)
			_, err := imp.store.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
			requirements.NoError(err)
			source, err := imp.store.GetOrCreateSource("slack", "T01:UME")
			requirements.NoError(err)
			requirements.NoError(imp.store.AddAccountIdentity(source.ID, "me@example.com", "manual"))
			sum, importErr := imp.Import(t.Context(), opts)
			requirements.NoError(importErr)
			wantMessages := 1
			if signal == "reply" {
				wantMessages = 2
			}
			assertions.Equal(wantMessages, sum.MessagesProcessed)
			assertions.Equal(conversationpolicy.Full, coldPolicyTier(t, imp.store, c.ID))
		})
	}
}

func coldPolicySlackdump(t *testing.T, count int, messages []any) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "room"), 0700))
	write := func(path string, value any) {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, path), data, 0600))
	}
	write("users.json", []any{map[string]any{"id": "USELF", "team_id": "TEAM", "profile": map[string]any{"email": "self@example.test"}}, map[string]any{"id": "UOTHER", "team_id": "TEAM", "profile": map[string]any{"email": "other@example.test"}}})
	members := []string{}
	for i := range count {
		member := fmt.Sprintf("user-%d", i)
		if i == 0 {
			member = "USELF"
		}
		if i == 1 {
			member = "UOTHER"
		}
		members = append(members, member)
	}
	write("channels.json", []any{map[string]any{"id": "ROOM", "name": "room", "num_members": count, "members": members}})
	write("room/2026-01-01.json", messages)
	return dir
}
func TestSlackdumpColdPolicyRetainsText(t *testing.T) {
	for _, tc := range []struct {
		name     string
		count    int
		disabled bool
		override conversationpolicy.Tier
		want     conversationpolicy.Tier
	}{
		{name: "twenty", count: 20, want: conversationpolicy.Full}, {name: "twenty-one", count: 21, want: conversationpolicy.Cold}, {name: "unknown", count: 0, want: conversationpolicy.Cold}, {name: "disabled", count: 21, disabled: true, want: conversationpolicy.Full}, {name: "explicit-full", count: 21, override: conversationpolicy.Full, want: conversationpolicy.Full}, {name: "explicit-cold", count: 20, override: conversationpolicy.Cold, want: conversationpolicy.Cold},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			dir := coldPolicySlackdump(t, tc.count, []any{map[string]any{"type": "message", "user": "UOTHER", "text": "retained text", "ts": "1704067200.000001", "files": []any{map[string]any{"id": "FILE", "name": "synthetic.txt", "size": 12, "mimetype": "text/plain"}}}})
			st := testutil.NewTestStore(t)
			settings := conversationpolicy.RecommendedSettings()
			if tc.disabled {
				settings.MaxParticipants["slack"] = 0
				settings.MaxParticipants["slackdump"] = 0
			}
			if tc.override != "" {
				settings.Overrides = []conversationpolicy.Override{{SourceType: "slackdump", SourceIdentifier: "TEAM:USELF", ConversationID: "ROOM", Tier: tc.override}}
			}
			_, err := st.ConfigureConversationPolicy(t.Context(), settings, time.Now())
			requirements.NoError(err)
			sum, err := ImportSlackdump(t.Context(), st, dir, SlackdumpImportOptions{Me: "self@example.test", MediaPolicy: attachmentpolicy.Policy{MaxParticipants: 1}})
			requirements.NoError(err)
			assertions.Equal(1, sum.MessagesProcessed)
			assertions.Equal(tc.want, coldPolicyTier(t, st, "ROOM"))
			assertions.Equal(1, sum.AttachmentsSkipped)
			var mediaReason string
			requirements.NoError(st.DB().QueryRow(`SELECT attachment_skip_reason FROM attachments`).Scan(&mediaReason))
			assertions.Equal(string(attachmentpolicy.SkipParticipantThreshold), mediaReason)
			assertions.Zero(sum.AttachmentsDownloaded)
			message, err := st.InspectMessage("ROOM:1704067200.000001")
			requirements.NoError(err)
			assertions.Equal("retained text", message.BodyText)
		})
	}
}
func TestSlackdumpColdPolicySameSyncActivityPromotion(t *testing.T) {
	for _, signal := range []string{"own", "mention", "reply"} {
		t.Run(signal, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			recent := fmt.Sprintf("%d.000100", time.Now().Add(-time.Hour).Unix())
			old := fmt.Sprintf("%d.000100", time.Now().Add(-181*24*time.Hour).Unix())
			message := map[string]any{"type": "message", "user": "UOTHER", "text": "recent", "ts": recent}
			messages := []any{message}
			switch signal {
			case "own":
				message["user"] = "USELF"
			case "mention":
				message["text"] = "hi <@USELF>"
			case "reply":
				message["thread_ts"] = old
				messages = append(messages, map[string]any{"type": "message", "user": "USELF", "text": "old own", "ts": old})
			}
			dir := coldPolicySlackdump(t, 21, messages)
			st := testutil.NewTestStore(t)
			_, policyErr := st.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
			require.NoError(t, policyErr)
			source, err := st.GetOrCreateSource("slackdump", "TEAM:USELF")
			requirements.NoError(err)
			requirements.NoError(st.AddAccountIdentity(source.ID, "TEAM:USELF", "manual"))
			sum, err := ImportSlackdump(t.Context(), st, dir, SlackdumpImportOptions{Me: "USELF"})
			requirements.NoError(err)
			assertions.Equal(len(messages), sum.MessagesProcessed)
			assertions.Equal(conversationpolicy.Full, coldPolicyTier(t, st, "ROOM"))
		})
	}
}

func TestImporterColdPolicyReplySweepActivityPromotion(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := testWorkspace(t)
	f.convs = f.convs[:1]
	c := f.convs[0]
	c.Members = nil
	for i := range 21 {
		c.Members = append(c.Members, fmt.Sprintf("U%02d", i))
	}
	c.Msgs = []fakeMsg{{TS: ts(0), User: "UALICE", Text: "quiet root"}}
	inserted := false
	replyTS := fmt.Sprintf("%d.000100", time.Now().Add(-5*time.Minute).Unix())
	f.onSearch = func(_ string, _ int) {
		if !inserted {
			inserted = true
			c.Msgs[0].Replies = []fakeMsg{{TS: replyTS, ThreadTS: ts(0), User: "UME", Text: "own reply discovered after history"}}
		}
	}
	imp, opts := testImporter(t, f)
	_, err := imp.store.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
	requirements.NoError(err)
	sum, err := imp.Import(t.Context(), opts)
	requirements.NoError(err)
	requirements.True(inserted)
	assertions.Equal(2, sum.MessagesAdded)
	assertions.Equal(conversationpolicy.Full, coldPolicyTier(t, imp.store, c.ID))
}
