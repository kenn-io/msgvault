package messageimport

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func snapshotFor(t *testing.T, edit func(*ImportedMessage)) Snapshot {
	t.Helper()
	req := validRequest(t)
	if edit != nil {
		edit(&req.Message)
	}
	normalized, err := req.Normalize()
	require.NoError(t, err)
	snapshot, err := BuildSnapshot(normalized)
	require.NoError(t, err)
	return snapshot
}

func TestBuildSnapshotRendersBodies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		edit     func(*ImportedMessage)
		wantText string
		wantHTML string
	}{
		{
			name:     "markdown is stored as text and ends with the url",
			wantText: "## Changes\n\n- Faster widgetsync for everyone.\n\nhttps://example.com/releases/1.2.0",
		},
		{
			name: "html keeps its markup and gains a text form",
			edit: func(m *ImportedMessage) {
				m.BodyMarkdown, m.BodyHTML = "", "<p>Faster <b>widgetsync</b> &amp; more.</p>"
				m.URL = "https://example.com/r?a=1&b=2"
			},
			wantText: "Faster widgetsync & more.\n\nhttps://example.com/r?a=1&b=2",
			wantHTML: "<p>Faster <b>widgetsync</b> &amp; more.</p>\n" +
				`<p><a href="https://example.com/r?a=1&amp;b=2">https://example.com/r?a=1&amp;b=2</a></p>`,
		},
		{
			name:     "text without a url is unchanged",
			edit:     func(m *ImportedMessage) { m.BodyMarkdown, m.BodyText, m.URL = "", "Plain words.", "" },
			wantText: "Plain words.",
		},
		{
			name:     "a title-only message stores the url as its body",
			edit:     func(m *ImportedMessage) { m.BodyMarkdown = "" },
			wantText: "https://example.com/releases/1.2.0",
		},
		{
			name: "a title-only message without a url has no body",
			edit: func(m *ImportedMessage) { m.BodyMarkdown, m.URL = "", "" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			snapshot := snapshotFor(t, tc.edit)
			assert.Equal(t, tc.wantText, snapshot.BodyText)
			assert.Equal(t, tc.wantHTML, snapshot.BodyHTML)
		})
	}
}

func TestBuildSnapshotStoresCanonicalRawAndMetadata(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)

	snapshot := snapshotFor(t, nil)
	assert.Equal(snapshotFor(t, nil).Raw, snapshot.Raw, "an identical request gives identical evidence")
	assert.Equal("entry-42", snapshot.SourceMessageID)
	assert.Equal("feed-7", snapshot.SourceConversationID)

	var raw map[string]any
	require.NoError(json.Unmarshal(snapshot.Raw, &raw))
	assert.Equal("2026-11-07T15:00:00Z", raw["sent_at"])
	assert.Equal("## Changes\n\n- Faster widgetsync for everyone.", raw["body_markdown"])
	assert.NotContains(string(snapshot.Raw), "account_email")

	var metadata map[string]any
	require.NoError(json.Unmarshal(snapshot.Metadata, &metadata))
	assert.Equal(SourceType, metadata["platform"])
	assert.Equal("feeds", metadata["source_identifier"])
	assert.Equal("entry-42", metadata["external_message_id"])
	assert.Equal("feed-7", metadata["external_conversation_id"])
	assert.Equal("https://example.com/releases/1.2.0", metadata["url"])
	provider, ok := metadata["provider_metadata"].(map[string]any)
	require.True(ok)
	assert.Equal("synthetic-hash", provider["entry_hash"])
}

func TestSenderIdentifier(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	assert.Equal("id:feeds:author-7",
		senderIdentifier("feeds", MessageImportSender{ID: "author-7", URL: "https://example.com", Name: "A"}),
		"an id wins and is scoped to its source")
	assert.Equal("url:https://example.com/Blog",
		senderIdentifier("feeds", MessageImportSender{URL: "https://EXAMPLE.com/Blog", Name: "A"}),
		"a url folds only its host's case")
	assert.Equal(
		senderIdentifier("feeds", MessageImportSender{URL: "https://example.com/Blog"}),
		senderIdentifier("bookmarks", MessageImportSender{URL: "https://example.com/Blog"}),
		"a url names the same sender in every source")
	assert.Equal("name:feeds:Example Project",
		senderIdentifier("feeds", MessageImportSender{Name: "Example Project"}),
		"a name is scoped to its source")
	assert.NotEqual(
		senderIdentifier("a:id:b", MessageImportSender{ID: "c"}),
		senderIdentifier("a", MessageImportSender{ID: "b:id:c"}),
		"a source identifier cannot imitate the separator")
}
