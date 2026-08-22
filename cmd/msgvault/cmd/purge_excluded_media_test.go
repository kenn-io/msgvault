package cmd

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type purgeMediaFixture struct {
	store       *store.Store
	config      *config.Config
	excludedID  int64
	retainedID  int64
	contentHash string
	contentPath string
	fullPath    string
}

func newPurgeMediaFixture(t *testing.T) purgeMediaFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	dataDir := t.TempDir()
	cfg := &config.Config{
		Data:   config.DataConfig{DataDir: dataDir},
		Beeper: config.BeeperConfig{MediaScope: string(attachmentpolicy.ScopeDirect)},
	}
	source, err := st.GetOrCreateSource(sourceTypeBeeper, "signal")
	require.NoError(t, err)
	newMessage := func(sourceMessageID, conversationType string, participants int) int64 {
		conversationID, err := st.EnsureConversationWithType(
			source.ID, "conversation-"+sourceMessageID, conversationType, sourceMessageID,
		)
		require.NoError(t, err)
		_, err = st.DB().Exec(st.Rebind(`UPDATE conversations SET participant_count = ? WHERE id = ?`), participants, conversationID)
		require.NoError(t, err)
		messageID, err := st.UpsertMessage(&store.Message{
			SourceID: source.ID, ConversationID: conversationID,
			SourceMessageID: sourceMessageID, MessageType: sourceTypeBeeper,
		})
		require.NoError(t, err)
		return messageID
	}
	excludedMessageID := newMessage("excluded", "channel", 20)
	retainedMessageID := newMessage("retained", "direct_chat", 2)
	hash := strings.Repeat("ab", 32)
	path := hash[:2] + "/" + hash
	for messageID, sourceAttachmentID := range map[int64]string{
		excludedMessageID: "beeper:excluded",
		retainedMessageID: "beeper:retained",
	} {
		require.NoError(t, st.ReplaceMessageBeeperAttachments(messageID, []store.AttachmentRef{{
			SourceAttachmentID: sourceAttachmentID,
			StoragePath:        path,
			ContentHash:        hash,
			Size:               100,
			State:              attachmentpolicy.StateStored,
		}}))
	}
	fullPath := filepath.Join(cfg.AttachmentsDir(), filepath.FromSlash(path))
	require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
	require.NoError(t, os.WriteFile(fullPath, []byte("shared attachment"), 0o600))

	attachmentID := func(messageID int64) int64 {
		var id int64
		require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT id FROM attachments WHERE message_id = ?`), messageID).Scan(&id))
		return id
	}
	return purgeMediaFixture{
		store: st, config: cfg,
		excludedID: attachmentID(excludedMessageID), retainedID: attachmentID(retainedMessageID),
		contentHash: hash, contentPath: path, fullPath: fullPath,
	}
}

func (f purgeMediaFixture) deps() purgeExcludedMediaDeps {
	return purgeExcludedMediaDeps{
		openStore:  func() (*store.Store, func(), error) { return f.store, func() {}, nil },
		config:     func() *config.Config { return f.config },
		removeFile: os.Remove,
	}
}

func (f purgeMediaFixture) state(t *testing.T, attachmentID int64) attachmentpolicy.DownloadState {
	t.Helper()
	var state sql.NullString
	require.NoError(t, f.store.DB().QueryRow(
		f.store.Rebind(`SELECT attachment_state FROM attachments WHERE id = ?`), attachmentID,
	).Scan(&state))
	return attachmentpolicy.DownloadState(state.String)
}

func TestPurgeExcludedMediaDryRunAndApplyPreservesSharedBlob(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPurgeMediaFixture(t)

	dryRun := newPurgeExcludedMediaLocalCmd(f.deps())
	var dryOutput bytes.Buffer
	dryRun.SetOut(&dryOutput)
	dryRun.SetErr(&dryOutput)
	dryRun.SetArgs([]string{"--dry-run"})
	require.NoError(dryRun.Execute())
	assert.Contains(dryOutput.String(), "Would exclude 1 stored attachment occurrence(s)")
	assert.Equal(attachmentpolicy.StateStored, f.state(t, f.excludedID))
	assert.Equal(attachmentpolicy.StateStored, f.state(t, f.retainedID))
	assert.FileExists(f.fullPath)

	apply := newPurgeExcludedMediaLocalCmd(f.deps())
	var applyOutput bytes.Buffer
	apply.SetOut(&applyOutput)
	apply.SetErr(&applyOutput)
	apply.SetArgs([]string{"--yes"})
	require.NoError(apply.Execute())
	assert.Contains(applyOutput.String(), "Excluded 1 stored attachment occurrence(s)")
	assert.Equal(attachmentpolicy.StateSkipped, f.state(t, f.excludedID))
	assert.Equal(attachmentpolicy.StateStored, f.state(t, f.retainedID))
	assert.FileExists(f.fullPath, "shared blob must remain while one stored occurrence references it")

	f.config.Beeper.MediaScope = string(attachmentpolicy.ScopeNone)
	removeLast := newPurgeExcludedMediaLocalCmd(f.deps())
	removeLast.SetOut(&bytes.Buffer{})
	removeLast.SetErr(&bytes.Buffer{})
	removeLast.SetArgs([]string{"--yes"})
	require.NoError(removeLast.Execute())
	assert.Equal(attachmentpolicy.StateSkipped, f.state(t, f.retainedID))
	assert.NoFileExists(f.fullPath)
}

func TestPurgeExcludedMediaPreservesBlobReferencedByThumbnail(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPurgeMediaFixture(t)
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		UPDATE attachments
		SET content_hash = NULL, storage_path = ?, thumbnail_hash = ?, thumbnail_path = ?
		WHERE id = ?
		RETURNING id
	`), "thumbnail-only", f.contentHash, f.contentPath, f.retainedID).Scan(&f.retainedID))

	command := newPurgeExcludedMediaLocalCmd(f.deps())
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--yes"})
	require.NoError(command.Execute())

	assert.Equal(attachmentpolicy.StateSkipped, f.state(t, f.excludedID))
	assert.FileExists(f.fullPath, "thumbnail references keep the shared loose blob live")
}

func TestPurgeExcludedMediaRefusalLeavesArchiveUnchanged(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPurgeMediaFixture(t)
	command := newPurgeExcludedMediaLocalCmd(f.deps())
	var output bytes.Buffer
	command.SetIn(strings.NewReader("n\n"))
	command.SetOut(&output)
	command.SetErr(&output)
	require.NoError(command.Execute())

	assert.Contains(output.String(), "Aborted.")
	assert.Equal(attachmentpolicy.StateStored, f.state(t, f.excludedID))
	assert.FileExists(f.fullPath)
}

func TestPurgeExcludedMediaRetriesAndContinuesLooseBlobCleanup(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newPurgeMediaFixture(t)
	f.config.Beeper.MediaScope = string(attachmentpolicy.ScopeNone)
	orphanHash := strings.Repeat("cd", 32)
	orphanPath := orphanHash[:2] + "/" + orphanHash
	orphanFullPath := filepath.Join(f.config.AttachmentsDir(), filepath.FromSlash(orphanPath))
	require.NoError(os.MkdirAll(filepath.Dir(orphanFullPath), 0o755))
	require.NoError(os.WriteFile(orphanFullPath, []byte("unreferenced attachment"), 0o600))

	deps := f.deps()
	deps.removeFile = func(path string) error {
		if path == f.fullPath {
			return errors.New("synthetic removal failure")
		}
		return os.Remove(path)
	}
	first := newPurgeExcludedMediaLocalCmd(deps)
	first.SetOut(&bytes.Buffer{})
	first.SetErr(&bytes.Buffer{})
	first.SetArgs([]string{"--yes"})
	require.Error(first.Execute())
	assert.Equal(attachmentpolicy.StateSkipped, f.state(t, f.excludedID))
	assert.Equal(attachmentpolicy.StateSkipped, f.state(t, f.retainedID))
	assert.FileExists(f.fullPath)
	assert.NoFileExists(orphanFullPath, "cleanup continues after an individual removal failure")

	retry := newPurgeExcludedMediaLocalCmd(f.deps())
	retry.SetOut(&bytes.Buffer{})
	retry.SetErr(&bytes.Buffer{})
	retry.SetArgs([]string{"--yes"})
	require.NoError(retry.Execute())
	assert.NoFileExists(f.fullPath, "a later run sweeps dead blobs even with no new exclusions")
}

// TestPurgeExcludedMediaRetainsUnresolvedRostersUnderParticipantLimit keeps
// the purge from deleting media on the strength of a roster nobody has
// archived — one a sync could not read, or one recorded before rosters were —
// because the accumulated participant rows may exceed the limit even when the
// current membership does not. Only the scope, account, and size rules apply
// until a sync records the roster, and the dry run says what was left in place.
func TestPurgeExcludedMediaRetainsUnresolvedRostersUnderParticipantLimit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	cfg := &config.Config{
		Data:  config.DataConfig{DataDir: t.TempDir()},
		Teams: config.TeamsConfig{MediaMaxParticipants: 4, MaxMediaMB: 1},
	}
	source, err := st.GetOrCreateSource(sourceTypeTeams, "me@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "team/channel", "channel", "Releases")
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE conversations SET participant_count = ? WHERE id = ?`), 20, conversationID)
	require.NoError(err)
	require.NoError(st.MarkConversationMemberCountUnknown(conversationID))
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: conversationID,
		SourceMessageID: "m1", MessageType: sourceTypeTeams,
	})
	require.NoError(err)
	smallHash := strings.Repeat("ab", 32)
	largeHash := strings.Repeat("cd", 32)
	require.NoError(st.ReplaceMessageInlineAttachments(messageID, []store.AttachmentRef{
		{
			SourceAttachmentID: "teams:inline:small", StoragePath: smallHash[:2] + "/" + smallHash,
			ContentHash: smallHash, Size: 100, State: attachmentpolicy.StateStored,
		},
		{
			SourceAttachmentID: "teams:inline:large", StoragePath: largeHash[:2] + "/" + largeHash,
			ContentHash: largeHash, Size: 2 << 20, State: attachmentpolicy.StateStored,
		},
	}, false))
	// A conversation archived before rosters were recorded: many accumulated
	// participants, no membership record at all.
	legacyID, err := st.EnsureConversationWithType(source.ID, "team/legacy", "channel", "Legacy")
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE conversations SET participant_count = ? WHERE id = ?`), 20, legacyID)
	require.NoError(err)
	legacyMessageID, err := st.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: legacyID,
		SourceMessageID: "m2", MessageType: sourceTypeTeams,
	})
	require.NoError(err)
	legacyHash := strings.Repeat("ef", 32)
	require.NoError(st.ReplaceMessageInlineAttachments(legacyMessageID, []store.AttachmentRef{{
		SourceAttachmentID: "teams:inline:legacy", StoragePath: legacyHash[:2] + "/" + legacyHash,
		ContentHash: legacyHash, Size: 100, State: attachmentpolicy.StateStored,
	}}, false))
	deps := purgeExcludedMediaDeps{
		openStore:  func() (*store.Store, func(), error) { return st, func() {}, nil },
		config:     func() *config.Config { return cfg },
		removeFile: os.Remove,
	}
	dryRun := func() string {
		cmd := newPurgeExcludedMediaLocalCmd(deps)
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetErr(&output)
		cmd.SetArgs([]string{"--dry-run"})
		require.NoError(cmd.Execute())
		return output.String()
	}

	output := dryRun()
	assert.Contains(output, "Would exclude 1 stored attachment occurrence(s)")
	assert.Contains(output, "size_cap: 1")
	assert.NotContains(output, string(attachmentpolicy.SkipParticipantThreshold))
	assert.Contains(output, "2 stored attachment occurrence(s) left in place: conversation membership is not archived",
		"the dry run must say what the participant limit could not evaluate")

	cfg.Teams.MediaScope = string(attachmentpolicy.ScopeDirect)
	output = dryRun()
	assert.Contains(output, "Would exclude 3 stored attachment occurrence(s)")
	assert.Contains(output, "policy_scope: 3")
	assert.NotContains(output, "left in place")
	cfg.Teams.MediaScope = ""

	// Rosters the sync has read make the participant limit authoritative.
	require.NoError(st.SetConversationMemberCount(conversationID, 20))
	require.NoError(st.SetConversationMemberCount(legacyID, 20))
	output = dryRun()
	assert.Contains(output, "Would exclude 3 stored attachment occurrence(s)")
	assert.Contains(output, "participant_threshold: 3")
	assert.NotContains(output, "left in place")
}
