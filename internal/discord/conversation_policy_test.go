package discord

import (
	"errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"strconv"
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

func TestImporterGuildCountForChatPolicyWithMediaDisabled(t *testing.T) {
	for _, count := range []int{20, 21, 0} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			assertions := assert.New(t)

			st := testutil.NewTestStore(t)
			_, err := st.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
			require.NoError(t, err)
			api := newImporterFakeAPI(importerTestChannel("300", "room"))
			api.guildMembers = count
			if count == 0 {
				api.guildCountErr = errors.New("membership unavailable")
			}
			api.messages["300"] = []Message{importerTestMessage("1001", "300", "retained text")}
			sum, err := newTestImporter(st, api).Import(t.Context(), ImportOptions{GuildID: "200"})
			require.NoError(t, err)
			assertions.Equal(1, api.guildCountRequests())
			assertions.EqualValues(1, sum.MessagesAdded)
			want := conversationpolicy.Cold
			if count == 20 {
				want = conversationpolicy.Full
			}
			assertions.Equal(want, coldPolicyTier(t, st, "300"))
		})
	}
}
func TestImporterColdPolicyRetainsTextAndMediaPolicy(t *testing.T) {
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

			st := testutil.NewTestStore(t)
			api := newImporterFakeAPI(importerTestChannel("300", "room"))
			api.guildMembers = tc.count
			message := importerTestMessage("1001", "300", "retained text")
			message.Attachments = []Attachment{testDiscordAttachment("https://cdn.discordapp.com/attachments/300/401/image.png", 16)}
			api.messages["300"] = []Message{message}
			settings := conversationpolicy.RecommendedSettings()
			if tc.disabled {
				settings.MaxParticipants["discord"] = 0
			}
			if tc.override != "" {
				settings.Overrides = []conversationpolicy.Override{{SourceType: "discord", SourceIdentifier: "200", ConversationID: "300", Tier: tc.override}}
			}
			_, err := st.ConfigureConversationPolicy(t.Context(), settings, time.Now())
			requirements.NoError(err)
			sum, err := newTestImporter(st, api).Import(t.Context(), ImportOptions{GuildID: "200", AttachmentsDir: t.TempDir(), MediaPolicy: attachmentpolicy.Policy{MaxParticipants: 1}})
			requirements.NoError(err)
			assertions.EqualValues(1, sum.MessagesAdded)
			assertions.EqualValues(1, sum.MediaSkipped)
			var mediaReason string
			requirements.NoError(st.DB().QueryRow(`SELECT attachment_skip_reason FROM attachments`).Scan(&mediaReason))
			assertions.Equal(string(attachmentpolicy.SkipParticipantThreshold), mediaReason)
			assertions.Zero(sum.MediaDownloaded)
			assertions.Equal(tc.want, coldPolicyTier(t, st, "300"))
			run, err := st.GetLatestSync(sum.SourceID)
			requirements.NoError(err)
			state, err := LoadSyncState(run.CursorAfter.String)
			requirements.NoError(err)
			assertions.True(state.Containers["300"].BackfillComplete)
			assertions.Equal("1001", state.Containers["300"].HighWater)
		})
	}
}
func TestImporterColdPolicySameSyncActivityPromotion(t *testing.T) {
	for _, signal := range []string{"own", "mention", "reply"} {
		t.Run(signal, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			st := testutil.NewTestStore(t)
			_, policyErr := st.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), time.Now())
			require.NoError(t, policyErr)
			api := newImporterFakeAPI(importerTestChannel("300", "room"))
			api.guildMembers = 21
			source, err := st.GetOrCreateSource("discord", "200")
			requirements.NoError(err)
			requirements.NoError(st.AddAccountIdentity(source.ID, "self-user", "manual"))
			recent := importerTestMessage("1002", "300", "recent")
			recent.Timestamp = time.Now().Add(-time.Hour)
			switch signal {
			case "own":
				recent.Author.ID = "self-user"
			case "mention":
				recent.Mentions = []User{{ID: "self-user"}}
			case "reply":
				recent.MessageReference = &MessageReference{MessageID: "1001", ChannelID: "300"}
			}
			parent := importerTestMessage("1001", "300", "old own")
			parent.Author.ID = "self-user"
			parent.Timestamp = time.Now().Add(-181 * 24 * time.Hour)
			api.messages["300"] = []Message{parent, recent}
			_, err = newTestImporter(st, api).Import(t.Context(), ImportOptions{GuildID: "200"})
			requirements.NoError(err)
			assertions.Equal(conversationpolicy.Full, coldPolicyTier(t, st, "300"))
			var count int
			requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
			assertions.Equal(2, count)
		})
	}
}
