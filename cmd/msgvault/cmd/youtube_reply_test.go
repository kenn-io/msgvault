package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/youtube"
)

func TestYouTubeReplyRejectsInvalidModesBeforeArchiveOrNetwork(t *testing.T) {
	for _, args := range [][]string{
		{"12", "--body", "Example reply"},
		{"12", "--body", "Example reply", "--dry-run", "--confirm", "token"},
		{"12", "--body", "Example reply", "--dry-run", "--resolve-comment", "sent-1"},
		{"12", "--body", "Example reply", "--resolve-comment", "sent-1"},
		{"0", "--body", "Example reply", "--dry-run"},
		{"bad", "--body", "Example reply", "--dry-run"},
		{"12", "--body", " ", "--dry-run"},
	} {
		t.Run(args[0]+args[len(args)-1], func(t *testing.T) {
			cmd := newYouTubeReplyCmd()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			require.NoError(t, cmd.ParseFlags(args))
			err := cmd.RunE(cmd, cmd.Flags().Args())
			require.Error(t, err)
			require.NotContains(t, err.Error(), "configuration is unavailable", "invalid invocations must fail before loading configuration")
		})
	}
}

func TestYouTubeReplyDryRunNeedsNoProviderCredentials(t *testing.T) {
	require := require.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Analytics.AutoBuildCache = false
	cfg.YouTube = []config.YouTubeSource{{Identifier: "example", ChannelID: "owner-1", TokenEnv: "EXAMPLE_YOUTUBE_REFRESH", ClientIDEnv: "EXAMPLE_YOUTUBE_CLIENT", ClientSecretEnv: "EXAMPLE_YOUTUBE_SECRET"}}
	// Every path points to this synthetic database; no implicit personal archive.
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	id := seedYouTubeReplyArchive(t, st)
	require.NoError(st.Close())
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv("EXAMPLE_YOUTUBE_REFRESH", "")
	t.Setenv("EXAMPLE_YOUTUBE_CLIENT", "")
	t.Setenv("EXAMPLE_YOUTUBE_SECRET", "")
	cmd := newYouTubeReplyCmd()
	cmd.SetContext(withInvocation(t.Context(), testInvocationWithConfig(cfg)))
	var output bytes.Buffer
	cmd.SetOut(&output)
	require.NoError(cmd.ParseFlags([]string{strconv.FormatInt(id, 10), "--body", "Example reply", "--dry-run"}))
	require.NoError(cmd.RunE(cmd, cmd.Flags().Args()))
	var receipt youtube.ReplyReceipt
	require.NoError(json.Unmarshal(output.Bytes(), &receipt))
	require.Equal("prepared", receipt.State)
	require.Equal("parent-1", receipt.Target.ParentID)
	require.NotEmpty(receipt.Token)
}

type acceptedYouTubeReplyProvider struct{ comment youtube.Comment }

func (p acceptedYouTubeReplyProvider) CheckReply(context.Context, youtube.ReplyTarget) error {
	return nil
}
func (p acceptedYouTubeReplyProvider) InsertReply(context.Context, string, string) (youtube.Comment, error) {
	return p.comment, nil
}
func (p acceptedYouTubeReplyProvider) GetReply(context.Context, string) (youtube.Comment, error) {
	return p.comment, nil
}

func TestYouTubeReplyAcceptedRecoveryNeedsNoOAuthCredentials(t *testing.T) {
	require := require.New(t)
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Analytics.AutoBuildCache = false
	cfg.YouTube = []config.YouTubeSource{{Identifier: "example", ChannelID: "owner-1", TokenEnv: "EXAMPLE_YOUTUBE_REFRESH", ClientIDEnv: "EXAMPLE_YOUTUBE_CLIENT", ClientSecretEnv: "EXAMPLE_YOUTUBE_SECRET"}}
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())
	id := seedYouTubeReplyArchive(t, st)
	target, err := youtube.ResolveReplyTarget(t.Context(), st, id)
	require.NoError(err)
	var comment youtube.Comment
	require.NoError(json.Unmarshal([]byte(`{"id":"sent-1","snippet":{"parentId":"parent-1","textOriginal":"Example reply","authorChannelId":{"value":"owner-1"}}}`), &comment))
	service := &youtube.ReplyService{Store: st, Provider: acceptedYouTubeReplyProvider{comment}, Persist: func(context.Context, youtube.Comment) (int64, error) { return 0, errors.New("fixture disk failure") }}
	preview, err := service.Preview(t.Context(), target, "Example reply")
	require.NoError(err)
	receipt, err := service.Confirm(t.Context(), target, "Example reply", preview.Token)
	require.Error(err)
	require.Equal("accepted", receipt.State)
	require.NoError(st.Close())
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	for _, name := range []string{"EXAMPLE_YOUTUBE_REFRESH", "EXAMPLE_YOUTUBE_CLIENT", "EXAMPLE_YOUTUBE_SECRET"} {
		t.Setenv(name, "")
	}
	for range 2 {
		cmd := newYouTubeReplyCmd()
		cmd.SetContext(withInvocation(t.Context(), testInvocationWithConfig(cfg)))
		var output bytes.Buffer
		cmd.SetOut(&output)
		require.NoError(cmd.ParseFlags([]string{strconv.FormatInt(id, 10), "--body", "Example reply", "--confirm", preview.Token}))
		require.NoError(cmd.RunE(cmd, cmd.Flags().Args()))
		var got youtube.ReplyReceipt
		require.NoError(json.Unmarshal(output.Bytes(), &got))
		require.Equal("archived", got.State)
		require.Positive(got.MessageID)
	}
}

func seedYouTubeReplyArchive(t *testing.T, st *store.Store) int64 {
	t.Helper()
	source, err := st.GetOrCreateSource("youtube", "https://www.youtube.com/channel/owner-1")
	require.NoError(t, err)
	rootID, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "video:video-1", MessageType: "youtube", Subject: sql.NullString{String: "Example video", Valid: true}, SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: "video-1", ConversationType: "group_chat", Title: "Example video"},
		Metadata:     &sql.NullString{Valid: true, String: `{"provider":"youtube","channel_id":"owner-1","video_id":"video-1"}`},
		RawMIME:      []byte(`{"id":"video-1","snippet":{"channelId":"owner-1","title":"Example video"}}`), RawFormat: "youtube_json",
	})
	require.NoError(t, err)
	root, err := st.GetMessageContext(t.Context(), rootID)
	require.NoError(t, err)
	id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: root.ConversationID, SourceMessageID: "parent-1", MessageType: "youtube", SentAt: sql.NullTime{Time: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Valid: true}})
	require.NoError(t, err)
	require.NoError(t, st.SetMessageMetadata(id, sql.NullString{Valid: true, String: `{"provider":"youtube","channel_id":"owner-1","video_id":"video-1","comment_id":"parent-1","top_level_comment_id":"parent-1","moderation_status":"published","thread_id":"thread-1","can_reply":true}`}))
	return id
}
