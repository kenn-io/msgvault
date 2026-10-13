package gvoice

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportGvoiceNotifyingAudioRetainsStrongerStoredOccurrence(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureSyntheticVoiceImports(t, st, true)
	voice := writeVoice(t, voicemailFixture{fixture: "with-audio.html", html: withAudioHTML, audio: withAudioMP3, bytes: []byte("Synthetic retained audio")})
	client, err := NewClient(voice, WithAttachmentsDir(t.TempDir()), WithNotifyImports(true))
	require.NoError(err)
	source, err := st.GetOrCreateSource("google_voice", client.Identifier())
	require.NoError(err)
	first, err := client.Import(t.Context(), st, source.ID)
	require.NoError(err)
	require.Equal(1, first.MessagesImported)
	before := attachmentRows(t, st)
	require.Len(before, 1)
	path := filepath.Join(voice, "Calls", withAudioHTML)
	html, err := os.ReadFile(path)
	require.NoError(err)
	require.NoError(os.WriteFile(path, []byte(strings.ReplaceAll(string(html), withAudioMP3, "missing.mp3")), 0o600))
	retry, err := client.Import(t.Context(), st, source.ID)
	require.NoError(err)
	require.Equal(1, retry.MessagesImported)
	assert.Equal(before, attachmentRows(t, st), "a failed acquisition retains the native stored occurrence, including metadata")
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Equal(1, events)
}

func TestImportGvoiceNotifyingAudioReplacementInvalidatesCache(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureSyntheticVoiceImports(t, st, true)
	voice := writeVoice(t, voicemailFixture{fixture: "with-audio.html", html: withAudioHTML, audio: withAudioMP3, bytes: []byte("Synthetic first audio")})
	client, err := NewClient(voice, WithAttachmentsDir(t.TempDir()), WithNotifyImports(true))
	require.NoError(err)
	source, err := st.GetOrCreateSource("google_voice", client.Identifier())
	require.NoError(err)
	first, err := client.Import(t.Context(), st, source.ID)
	require.NoError(err)
	require.Equal(1, first.MessagesImported)
	before, err := st.DerivedDataRevision()
	require.NoError(err)
	require.NoError(os.WriteFile(filepath.Join(voice, "Calls", withAudioMP3), []byte("Synthetic changed audio bytes"), 0o600))
	retry, err := client.Import(t.Context(), st, source.ID)
	require.NoError(err)
	require.Equal(1, retry.MessagesImported)
	after, err := st.DerivedDataRevision()
	require.NoError(err)
	assert.Greater(after, before, "an existing native attachment update invalidates archive projections")
	stable, err := client.Import(t.Context(), st, source.ID)
	require.NoError(err)
	require.Equal(1, stable.MessagesImported)
	unchanged, err := st.DerivedDataRevision()
	require.NoError(err)
	assert.Equal(after, unchanged, "an unchanged repeat does not invalidate cache")
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Equal(1, events)
}
