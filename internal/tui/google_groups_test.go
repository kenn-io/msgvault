package tui

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestGoogleGroupsLabelsSanitizedAtDisplay(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	label := "Résolu\x1b[2J\x1b]52;c;eA==\x07\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\"
	raw := fmt.Sprintf("From synthetic@example.invalid Mon Jan 1 12:00:00 +0000 2024\r\nFrom: Alice <alice@example.com>\r\nSubject: Topic\r\nX-Google-Groups: test-group\r\nX-Gmail-Labels: =?UTF-8?B?%s?=\r\n\r\nSynthetic message.\r\n", base64.StdEncoding.EncodeToString([]byte(label)))
	path := filepath.Join(t.TempDir(), "topics.mbox")
	require.NoError(os.WriteFile(path, []byte(raw), 0600))
	summary, err := importer.ImportMbox(t.Context(), st, path, importer.MboxImportOptions{
		SourceType: "google-groups", Identifier: "test-group@googlegroups.com",
	})
	require.NoError(err)
	require.Equal(int64(1), summary.MessagesAdded)
	var id int64
	require.NoError(st.DB().QueryRow("SELECT id FROM messages").Scan(&id))
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	detail, err := engine.GetMessage(t.Context(), id)
	require.NoError(err)
	require.Contains(detail.Labels, label, "archive reads retain the original label")

	model := NewBuilder().WithSize(120, 30).WithLevel(levelMessageDetail).Build()
	model.messageDetail = detail
	output := model.View().Content
	assert.NotContains(output, "\x1b[2J")
	assert.NotContains(output, "\x1b]52;")
	assert.NotContains(output, "\x1b]8;")
	assert.Contains(output, "Résolulink")
	assert.Contains(detail.Labels, label, "rendering must not mutate the original label")
}
