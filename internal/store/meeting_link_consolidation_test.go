package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Seed the authoritative decision rows directly: consolidation must preserve
// imported decisions too, independently of the later read-time link evaluator.
func TestDecisionConsolidationCollisionNewestWins(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	legacyData, legacy := projectionFixture(t, st, "legacy", "granola_json", `{}`)
	_, survivor := projectionFixture(t, st, "canonical", "granola_json", `{}`)
	targetData, target := projectionFixture(t, st, "target", "gcal_json", `{"id":"target","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	targetData.Message.MessageType = "calendar_event"
	_, err := st.UpsertMessage(targetData.Message)
	requirements.NoError(err)
	ref := fmt.Sprintf("evt:%d", target)
	_, err = st.db.Exec(`INSERT INTO meeting_link_decisions(member_message_id,target_ref,target_logical,decision,created_at) VALUES (?, ?, '', 'link', ?)`, legacy, ref, time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC))
	requirements.NoError(err)
	_, err = st.db.Exec(`INSERT INTO meeting_link_decisions(member_message_id,target_ref,target_logical,decision,created_at) VALUES (?, ?, '', 'split', ?)`, survivor, ref, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	requirements.NoError(err)
	staleRef := "evt:9223372036854770000"
	_, err = st.db.Exec(`INSERT INTO meeting_link_decisions(member_message_id,target_ref,target_logical,decision,created_at) VALUES (?, ?, '', 'split', ?)`, legacy, staleRef, time.Date(2027, 1, 3, 0, 0, 0, 0, time.UTC))
	requirements.NoError(err)
	requirements.NoError(st.MigrateSourceMessageID(legacyData.Message.SourceID, legacyData.Message.ConversationID, "legacy", "canonical"))
	var decision string
	requirements.NoError(st.db.QueryRow(`SELECT decision FROM meeting_link_decisions WHERE member_message_id = ? AND target_ref = ?`, survivor, ref).Scan(&decision))
	assertions.Equal("link", decision)
	var staleTarget sql.NullInt64
	requirements.NoError(st.db.QueryRow(`SELECT target_message_id FROM meeting_link_decisions WHERE member_message_id = ? AND target_ref = ?`, survivor, staleRef).Scan(&staleTarget))
	assertions.False(staleTarget.Valid, "consolidation must preserve unresolved legacy refs without inventing a foreign-key target")
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM meeting_link_decisions WHERE member_message_id = ?`, legacy).Scan(&count))
	assertions.Zero(count)
}

func TestDecisionConsolidationNewerEquivalentSplitWins(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	a, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	b, _ := calendarLinkFixture(t, st, "copy@example.org", linkSeriesRaw)
	legacyData, legacy := projectionFixture(t, st, "legacy-equivalent", "plaud_json", `{}`)
	_, survivor := projectionFixture(t, st, "canonical-equivalent", "plaud_json", `{}`)
	refA := fmt.Sprintf("evt:%d@20270314T100000Z", a)
	refB := fmt.Sprintf("evt:%d@20270314T100000Z", b)
	requirements.NoError(st.LinkMeetingContext(t.Context(), refA, legacy))
	requirements.NoError(st.UnlinkMeetingContext(t.Context(), refB, survivor))
	_, err := st.db.Exec(`UPDATE meeting_link_decisions SET created_at = ? WHERE member_message_id = ? AND target_ref = ?`, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), legacy, refA)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE meeting_link_decisions SET created_at = ? WHERE member_message_id = ? AND target_ref = ?`, time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC), survivor, refB)
	requirements.NoError(err)

	requirements.NoError(st.MigrateSourceMessageID(legacyData.Message.SourceID, legacyData.Message.ConversationID, "legacy-equivalent", "canonical-equivalent"))
	assertions.Empty(memberLinks(t, st, OwnerScope(), survivor), "a newer split must continue to veto an older link after member consolidation")
	var linkCount int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM meeting_link_decisions WHERE member_message_id = ? AND decision = 'link' AND target_logical = ?`, survivor, "ical:weekly@example.org@20270314T100000Z").Scan(&linkCount))
	assertions.Zero(linkCount)
}

func TestDecisionConsolidationRepointsTargetAndPreservesKey(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	legacyData, legacy := projectionFixture(t, st, "legacy-event", "gcal_json", `{"id":"legacy","iCalUID":"event@example.org","start":{"dateTime":"2027-03-07T10:00:00Z"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`)
	survivorData, survivor := projectionFixture(t, st, "canonical-event", "gcal_json", `{"id":"canonical","iCalUID":"event@example.org","start":{"dateTime":"2027-03-07T10:00:00Z"},"end":{"dateTime":"2027-03-07T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=6"]}`)
	legacyData.Message.MessageType = "calendar_event"
	_, err := st.UpsertMessage(legacyData.Message)
	requirements.NoError(err)
	survivorData.Message.MessageType = "calendar_event"
	_, err = st.UpsertMessage(survivorData.Message)
	requirements.NoError(err)
	_, note := projectionFixture(t, st, "manual-note", "plaud_json", `{}`)
	oldRef := fmt.Sprintf("evt:%d@20270314T100000Z", legacy)
	_, err = st.db.Exec(`INSERT INTO meeting_link_decisions(member_message_id,target_ref,target_logical,decision,created_at) VALUES (?, ?, ?, 'split', ?)`, note, oldRef, "ical:event@example.org@20270314T100000Z", time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC))
	requirements.NoError(err)
	requirements.NoError(st.MigrateSourceMessageID(legacyData.Message.SourceID, legacyData.Message.ConversationID, "legacy-event", "canonical-event"))
	var ref, logical string
	requirements.NoError(st.db.QueryRow(`SELECT target_ref,target_logical FROM meeting_link_decisions WHERE member_message_id = ?`, note).Scan(&ref, &logical))
	assertions.Equal(fmt.Sprintf("evt:%d@20270314T100000Z", survivor), ref)
	assertions.Equal("ical:event@example.org@20270314T100000Z", logical)
}

func TestDecisionConsolidationWaitsForConcurrentDecision(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("requires PostgreSQL row locks")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	legacyData, legacy := projectionFixture(t, st, "legacy", "plaud_json", `{}`)
	_, survivor := projectionFixture(t, st, "canonical", "plaud_json", `{}`)
	targetData, target := projectionFixture(t, st, "target", "gcal_json", `{"id":"target","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	targetData.Message.MessageType = "calendar_event"
	_, err := st.UpsertMessage(targetData.Message)
	requirements.NoError(err)

	writer, err := st.db.BeginTx(ctx, nil)
	requirements.NoError(err)
	writerOpen := true
	defer func() {
		if writerOpen {
			_ = writer.Rollback()
		}
	}()
	requirements.NoError(st.lockMeetingDecisionEvidenceMessageWith(ctx, writer, legacy))
	_, err = writer.ExecContext(ctx, `INSERT INTO meeting_link_decisions(member_message_id,target_ref,target_logical,decision,created_at) VALUES (?, ?, '', 'link', ?)`, legacy, fmt.Sprintf("evt:%d", target), time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC))
	requirements.NoError(err)

	lockAttempt, decisionScan := make(chan struct{}, 1), make(chan struct{}, 1)
	signal := func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	migrator := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, query string, args []driver.NamedValue) error {
		normalized := strings.Join(strings.Fields(query), " ")
		if strings.Contains(normalized, "SELECT id FROM messages WHERE id =") && strings.Contains(normalized, "FOR UPDATE") && len(args) > 0 && args[0].Value == legacy {
			signal(lockAttempt)
		}
		if strings.Contains(normalized, "FROM meeting_link_decisions WHERE member_message_id =") {
			signal(decisionScan)
		}
		return nil
	}})
	migrationDone := make(chan error, 1)
	go func() {
		migrationDone <- migrator.MigrateSourceMessageID(legacyData.Message.SourceID, legacyData.Message.ConversationID, "legacy", "canonical")
	}()

	first := "timeout"
	select {
	case <-lockAttempt:
		first = "message lock"
	case <-decisionScan:
		first = "decision scan"
	case <-ctx.Done():
	}
	commitErr := writer.Commit()
	writerOpen = false
	requirements.NoError(commitErr)
	select {
	case err := <-migrationDone:
		requirements.NoError(err)
	case <-time.After(20 * time.Second):
		requirements.FailNow("source-message migration did not finish after the decision writer committed")
	}

	assertions.Equal("message lock", first, "consolidation must acquire the legacy member lock before scanning decisions")
	var member int64
	var ref string
	requirements.NoError(st.db.QueryRow(`SELECT member_message_id,target_ref FROM meeting_link_decisions WHERE member_message_id = ?`, survivor).Scan(&member, &ref))
	assertions.Equal(survivor, member)
	assertions.Equal(fmt.Sprintf("evt:%d", target), ref)
}

func TestDecisionConsolidationSerializesTargetWriter(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("requires PostgreSQL row locks")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	legacyData, legacy := projectionFixture(t, st, "legacy-event", "gcal_json", `{"id":"legacy-event","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	survivorData, survivor := projectionFixture(t, st, "canonical-event", "gcal_json", `{"id":"canonical-event","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	for _, data := range []*MessagePersistData{legacyData, survivorData} {
		data.Message.MessageType = "calendar_event"
		_, err := st.UpsertMessage(data.Message)
		requirements.NoError(err)
	}
	_, member := projectionFixture(t, st, "manual-note", "plaud_json", `{}`)
	requirements.Less(legacy, member, "the legacy target sorts before the decision member")
	ref := fmt.Sprintf("evt:%d", legacy)

	deleteAttempt, allowDelete := make(chan struct{}, 1), make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(allowDelete)
		}
	}()
	signal := func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	migrator := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, query string, _ []driver.NamedValue) error {
		if strings.HasPrefix(query, "DELETE FROM messages WHERE source_id =") {
			signal(deleteAttempt)
			<-allowDelete
		}
		return nil
	}})
	migrationDone := make(chan error, 1)
	go func() {
		migrationDone <- migrator.MigrateSourceMessageID(legacyData.Message.SourceID, legacyData.Message.ConversationID, "legacy-event", "canonical-event")
	}()
	select {
	case <-deleteAttempt:
	case err := <-migrationDone:
		requirements.NoError(err)
		requirements.FailNow("source-message migration finished before reaching its gated delete")
	case <-ctx.Done():
		requirements.FailNow("source-message migration did not reach its gated delete")
	}

	targetLockAttempt, decisionWriteAttempt := make(chan struct{}, 1), make(chan struct{}, 1)
	writer := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, query string, args []driver.NamedValue) error {
		if strings.Contains(query, "SELECT id FROM messages WHERE id =") && strings.Contains(query, "FOR NO KEY UPDATE") && len(args) > 0 && args[0].Value == legacy {
			signal(targetLockAttempt)
		}
		if strings.Contains(query, "INSERT INTO meeting_link_decisions") {
			signal(decisionWriteAttempt)
		}
		return nil
	}})
	decisionDone := make(chan error, 1)
	go func() {
		decisionDone <- writer.LinkMeetingContext(ctx, ref, member)
	}()
	first := "timeout"
	var decisionErr error
	decisionFinished := false
	select {
	case <-targetLockAttempt:
		first = "target lock"
	case <-decisionWriteAttempt:
		first = "decision write"
	case decisionErr = <-decisionDone:
		first = "finished"
		decisionFinished = true
	case <-ctx.Done():
	}
	if first == "decision write" {
		select {
		case decisionErr = <-decisionDone:
			decisionFinished = true
		case <-ctx.Done():
		}
	}
	close(allowDelete)
	released = true
	select {
	case err := <-migrationDone:
		requirements.NoError(err)
	case <-ctx.Done():
		requirements.FailNow("source-message migration did not finish after its gated delete was released")
	}
	if !decisionFinished {
		select {
		case decisionErr = <-decisionDone:
		case <-ctx.Done():
			requirements.FailNow("target decision did not finish after consolidation committed")
		}
	}

	assertions.Equal("target lock", first, "a decision writer must lock its target before writing")
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM meeting_link_decisions WHERE member_message_id = ? AND target_ref = ?`, member, ref).Scan(&count))
	assertions.Zero(count, "a decision must not survive with a deleted target message")
	requirements.ErrorIs(decisionErr, sql.ErrNoRows)
	assertions.NotEqual(legacy, survivor)
}

func TestDecisionConsolidationNoDeadlockWhenMemberIDLowerThanTarget(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("requires PostgreSQL row locks")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	_, member := projectionFixture(t, st, "manual-note", "plaud_json", `{}`)
	legacy, sourceID := calendarLinkFixture(t, st, "primary@example.com", `{"id":"legacy-event","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	survivor, _ := calendarLinkFixture(t, st, "primary@example.com", `{"id":"canonical-event","iCalUID":"shared@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	requirements.Less(member, legacy, "the decision member must sort before the legacy target")
	var conversationID int64
	requirements.NoError(st.db.QueryRow(`SELECT conversation_id FROM messages WHERE id = ?`, legacy).Scan(&conversationID))
	var foundLegacy, foundSurvivor int64
	requirements.NoError(st.db.QueryRow(`SELECT id FROM messages WHERE source_id = ? AND conversation_id = ? AND source_message_id = ?`, sourceID, conversationID, "legacy-event").Scan(&foundLegacy))
	requirements.NoError(st.db.QueryRow(`SELECT id FROM messages WHERE source_id = ? AND source_message_id = ?`, sourceID, "canonical-event").Scan(&foundSurvivor))
	assertions.Equal(legacy, foundLegacy)
	assertions.Equal(survivor, foundSurvivor)
	ref := fmt.Sprintf("evt:%d", legacy)
	_, err := st.db.Exec(`INSERT INTO meeting_link_decisions(member_message_id,target_ref,target_logical,decision,created_at) VALUES (?, ?, '', 'link', ?)`, member, ref, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	requirements.NoError(err)

	repointAttempt, allowRepoint := make(chan struct{}, 1), make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(allowRepoint)
		}
	}()
	signal := func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	migrator := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(ctx context.Context, query string, _ []driver.NamedValue) error {
		if strings.HasPrefix(query, "DELETE FROM meeting_link_decisions WHERE member_message_id = ") {
			signal(repointAttempt)
			select {
			case <-allowRepoint:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}})
	migrationDone := make(chan error, 1)
	go func() {
		migrationDone <- migrator.MigrateSourceMessageID(sourceID, conversationID, "legacy-event", "canonical-event")
	}()
	select {
	case <-repointAttempt:
	case err := <-migrationDone:
		requirements.NoError(err)
		requirements.FailNow("consolidation finished without reaching the seeded decision repoint")
	case <-ctx.Done():
		requirements.FailNow("consolidation did not reach the seeded decision repoint")
	}

	targetLockAttempt := make(chan struct{}, 1)
	writer := gatedMeetingProjectionStore(t, st, &meetingProjectionGate{before: func(_ context.Context, query string, args []driver.NamedValue) error {
		if strings.Contains(query, "SELECT id FROM messages WHERE id =") && strings.Contains(query, "FOR NO KEY UPDATE") && len(args) > 0 && args[0].Value == legacy {
			signal(targetLockAttempt)
		}
		return nil
	}})
	decisionDone := make(chan error, 1)
	go func() {
		decisionDone <- writer.LinkMeetingContext(ctx, ref, member)
	}()
	select {
	case <-targetLockAttempt:
		// The writer reaches the target only after it has acquired the lower-ID member.
	case err := <-decisionDone:
		requirements.NoError(err)
		requirements.FailNow("decision writer finished before attempting the legacy target lock")
	case <-ctx.Done():
		requirements.FailNow("decision writer did not attempt the legacy target lock")
	}

	close(allowRepoint)
	released = true
	var migrationErr, decisionErr error
	select {
	case migrationErr = <-migrationDone:
	case <-ctx.Done():
		requirements.FailNow("target consolidation deadlocked while repointing the decision")
	}
	select {
	case decisionErr = <-decisionDone:
	case <-ctx.Done():
		requirements.FailNow("decision writer did not finish after target consolidation")
	}
	requirements.NoError(migrationErr)
	requirements.ErrorIs(decisionErr, sql.ErrNoRows)

	var gotMember int64
	var gotRef string
	requirements.NoError(st.db.QueryRow(`SELECT member_message_id,target_ref FROM meeting_link_decisions WHERE member_message_id = ?`, member).Scan(&gotMember, &gotRef))
	assertions.Equal(member, gotMember)
	assertions.Equal(fmt.Sprintf("evt:%d", survivor), gotRef)
}
