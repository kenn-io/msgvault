package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

// This exercises the existing packet and ledger, without a provider or model.
// X is not an authenticated sender source and cannot produce durable person
// fact evidence through the ordinary production validation path.
func TestXRemovalKeepsUnsupportedPersonEvidenceOutOfLedger(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	st, personID := newPersonFactLedgerStore(t)
	source, err := st.GetOrCreateSource("x", "1001")
	_testRequire.NoError(err)
	at := time.Now().UTC()
	saved, err := st.PersistXPostContext(t.Context(), XPersistPostInput{SourceID: source.ID, OwnerID: "1001", PostID: "100", AuthorID: "1002", ConversationID: "100", EditHistory: []string{"100"}, Text: "retained constellation excerpt", CreatedAt: at, FetchedAt: at, Availability: "public", Author: &XAuthorInput{UserID: "1002", Username: "example_author", DisplayName: "Example Author", FetchedAt: at}})
	_testRequire.NoError(err)
	var pid int64
	_testRequire.NoError(st.DB().QueryRow(st.Rebind(`SELECT sender_id FROM messages WHERE id=?`), saved.MessageID).Scan(&pid))
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO person_participants(person_id,participant_id) VALUES(?,?)`), personID, pid)
	_testRequire.NoError(err)
	_, err = st.SetPersonTrackingContext(t.Context(), personID, true)
	_testRequire.NoError(err)
	seq, err := st.LatestPersonSweepChangeSequence(t.Context())
	_testRequire.NoError(err)
	window, err := st.LoadPersonSweepWindow(t.Context(), peoplesweep.WindowRequest{PersonID: personID, Lane: peoplesweep.SourceConversationText, ThroughSequence: seq, Limit: 100, Mode: peoplesweep.GenerationCursorBackstop})
	_testRequire.NoError(err)
	_testRequire.Len(window.Seeds, 1)
	// X is intentionally absent from the authenticated-sender allowlist.
	// The real evidence validator prevents its indirect text becoming a durable
	// person-fact excerpt or a brief citation.
	_, err = peoplesweep.PersonFactEvidenceInput(window.Seeds[0])
	_testRequire.ErrorContains(err, "subject person id")
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source.ID)
	_testRequire.NoError(err)
	defer func() { assert.NoError(t, execution.Release()) }()
	work, err := st.RemoveXPostContentContext(t.Context(), source.ID, "100", "removed")
	_testRequire.NoError(err)
	body, err := st.GetMessageBodyText(saved.MessageID)
	_testRequire.NoError(err)
	_testAssert.Empty(body)
	work.DerivedComplete = true
	_testRequire.NoError(st.FinishXRemovalContext(t.Context(), source.ID, work))
	evidence, err := st.ListPersonFactEvidenceContext(t.Context(), personID, personfacts.EvidenceFilter{})
	_testRequire.NoError(err)
	_testAssert.Empty(evidence)
}
