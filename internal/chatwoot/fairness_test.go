package chatwoot

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBudgetRotatesPendingArtifacts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var messages []map[string]any
	for id := int64(101); id <= 103; id++ {
		m := contractMessage(id, 1767225600+id, nil)
		m["attachments"] = []any{map[string]any{"id": id, "file_type": "audio"}}
		messages = append(messages, m)
	}
	api := newContractAPI(t, 1000, messages)
	st := testutil.NewTestStore(t)
	imp, _ := contractRegister(t, st, api)
	_, err := imp.Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
	require.NoError(err)
	func() {
		api.mu.Lock()
		defer api.mu.Unlock()
		for _, m := range api.messages {
			attachments, ok := m["attachments"].([]any)
			require.True(ok)
			require.NotEmpty(attachments)
			attachment, ok := attachments[0].(map[string]any)
			require.True(ok)
			attachment["transcribed_text"] = "corrected"
		}
	}()
	// Each run lists two activity pages, leaving two refreshes.
	imp.requestBudget = 4
	for range 2 {
		_, err = imp.Import(t.Context(), ImportOptions{InboxID: 7, IncludePrivate: true})
		require.NoError(err)
	}
	for id := int64(101); id <= 103; id++ {
		body, err := st.GetMessageBodyText(contractArchivedMessageID(t, st, strconv.FormatInt(id, 10)))
		require.NoError(err)
		assert.Contains(body, "corrected")
	}
}
