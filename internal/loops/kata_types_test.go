package loops

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultTrackTitleUsesKindAndBoundsText(t *testing.T) {
	for _, tc := range []struct {
		kind, name, want string
		participant      int64
	}{
		{"reply_owed", "Example Person", "Reply to Example Person: A question", 0},
		{"awaiting_reply", "Example Person", "Waiting on Example Person: A question", 0},
		{"reply_owed", "", "Reply to participant 42: A question", 42},
		{"commitment_owed_by_me", "Example Person", "A question", 0},
	} {
		t.Run(tc.want, func(t *testing.T) {
			assert.Equal(t, tc.want, DefaultTrackTitle(Loop{Kind: tc.kind, Summary: "A question", Counterparty: Counterparty{DisplayName: tc.name, ParticipantID: tc.participant}}))
		})
	}
	title := DefaultTrackTitle(Loop{Kind: "reply_owed", Summary: strings.Repeat("界", 600), Counterparty: Counterparty{DisplayName: "Example"}})
	assert.LessOrEqual(t, len([]rune(title)), 512)
}

func TestKataReceiptWireFields(t *testing.T) {
	data, err := json.Marshal(KataIssue{UID: "issue-uid", Ref: "ab12", QualifiedRef: "example#ab12", Project: "example", Title: "A question", Status: "open", Revision: "3", WebURL: "https://example.test/issues/ab12"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"uid":"issue-uid","ref":"ab12","qualified_ref":"example#ab12","project":"example","title":"A question","status":"open","revision":"3","web_url":"https://example.test/issues/ab12"}`, string(data))
	var loop Loop
	data, err = json.Marshal(loop)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"kata":null`)
	assert.Contains(t, string(data), `"closed_by":null`)
}

func TestLoopEvidencePreservesKataDiagnostics(t *testing.T) {
	var evidence Evidence
	require.NoError(t, json.Unmarshal([]byte(`{"kata_closed_ref":"example#ab12","kata_reopen_blocked":"42"}`), &evidence))
	data, err := json.Marshal(evidence)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"kata_closed_ref":"example#ab12"`)
	assert.Contains(t, string(data), `"kata_reopen_blocked":"42"`)
}
