package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

func policyFactFixture(t *testing.T) (*Store, int64, int64, int64, personfacts.ProposedClaim) {
	t.Helper()
	s, person, targets := newPersonFactProjectionStore(t)
	pid, err := s.EnsureParticipant("policy@example.test", "Policy Person", "example.test")
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO person_participants(person_id,participant_id) VALUES(?,?)`, person, pid)
	require.NoError(t, err)
	source, err := s.GetOrCreateSource("slack", "policy-facts")
	require.NoError(t, err)
	room, err := s.EnsureConversation(source.ID, "policy-room", "Policy Room")
	require.NoError(t, err)
	require.NoError(t, s.SetConversationPolicyOverride(t.Context(), source.ID, "policy-room", conversationpolicy.Full))
	id, err := s.UpsertMessage(&Message{SourceID: source.ID, SourceMessageID: "fact-message", ConversationID: room, MessageType: "chat", SenderID: sql.NullInt64{Int64: pid, Valid: true}, SentAt: sql.NullTime{Time: time.Now().UTC().Truncate(time.Second), Valid: true}})
	require.NoError(t, err)
	_, err = s.db.Exec(`INSERT INTO message_bodies(message_id,body_text) VALUES(?,?)`, id, "I prefer chat for project discussions.")
	require.NoError(t, err)
	seeds, _, err := s.hydratePersonSweepMessageSet(t.Context(), person, peoplesweep.SourceConversationText, []int64{id}, 0)
	require.NoError(t, err)
	require.Len(t, seeds, 1)
	evidence, err := peoplesweep.PersonFactEvidenceInput(seeds[0])
	require.NoError(t, err)
	alignment, err := (PersonSweepEvidenceAligner{Store: s}).Align(t.Context(), evidence)
	require.NoError(t, err)
	require.True(t, alignment.Accepted, "%+v", alignment.Failure)
	claim := personFactProjectionClaim(person, targets[AttributeSlugPrimaryChannel], `"chat"`, "policy")
	claim.Evidence = []personfacts.EvidenceInput{evidence}
	input := personFactProjectionInput(person, "policy-initial", []personfacts.ProposedClaim{claim}, nil)
	input.ResolvedAt = time.Now().UTC()
	result, err := s.ApplyPersonFactGenerationContext(t.Context(), input, PersonSweepEvidenceAligner{Store: s})
	require.NoError(t, err)
	require.NotEmpty(t, result.Projections)
	return s, person, source.ID, id, claim
}
func policyFactValues(t *testing.T, s *Store, person int64) []PersonAttributeValue {
	t.Helper()
	values, err := s.ListPersonAttributeValuesContext(t.Context(), person, PersonAttributeQuery{DefinitionSlug: AttributeSlugPrimaryChannel})
	require.NoError(t, err)
	return values
}
func TestConversationPolicyPeopleMixedEvidenceRetraction(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, person, source, _, _ := policyFactFixture(t)
	before, err := s.LoadPersonSemanticDocumentContext(t.Context(), person)
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "policy-room", conversationpolicy.Cold))
	assertions.Empty(policyFactValues(t, s, person))
	after, err := s.LoadPersonSemanticDocumentContext(t.Context(), person)
	requirements.NoError(err)
	assertions.NotEqual(before.Revision, after.Revision)
}
func TestConversationPolicyPeoplePromotionReplaysEvidence(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, person, source, _, _ := policyFactFixture(t)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "policy-room", conversationpolicy.Cold))
	assertions.Empty(policyFactValues(t, s, person))
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "policy-room", conversationpolicy.Full))
	assertions.Len(policyFactValues(t, s, person), 1)
}
func TestConversationPolicyPromotionDoesNotResurrectDeletedEvidence(t *testing.T) {
	for _, reason := range []personfacts.EvidenceStatusReason{personfacts.EvidenceStatusSourceDeleted, personfacts.EvidenceStatusSourceEdited, personfacts.EvidenceStatusScopeUnlinked, personfacts.EvidenceStatusIdentityReassigned} {
		t.Run(string(reason), func(t *testing.T) {
			requirements := require.New(t)

			s, person, source, _, _ := policyFactFixture(t)
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "policy-room", conversationpolicy.Cold))
			evidence, err := s.ListPersonFactEvidenceContext(t.Context(), person, personfacts.EvidenceFilter{})
			requirements.NoError(err)
			requirements.Len(evidence, 1)
			input := personFactProjectionInput(person, "withdrawal", nil, []personfacts.EvidenceStatusChange{{EvidenceKey: evidence[0].Key, SourceVersion: evidence[0].Input.SourceVersion, Reason: reason, Supported: false}})
			input.ResolvedAt = time.Now().UTC()
			_, err = s.ApplyPersonFactGenerationContext(t.Context(), input, nil)
			requirements.NoError(err)
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "policy-room", conversationpolicy.Full))
			assert.Empty(t, policyFactValues(t, s, person))
		})
	}
}
func TestConversationPolicyReimportCannotReactivateColdEvidence(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, person, source, _, _ := policyFactFixture(t)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "policy-room", conversationpolicy.Cold))
	evidence, err := s.ListPersonFactEvidenceContext(t.Context(), person, personfacts.EvidenceFilter{})
	requirements.NoError(err)
	requirements.Len(evidence, 1)
	before, err := s.LoadPersonSemanticDocumentContext(t.Context(), person)
	requirements.NoError(err)
	input := personFactProjectionInput(person, "reimport", nil, []personfacts.EvidenceStatusChange{{EvidenceKey: evidence[0].Key, SourceVersion: evidence[0].Input.SourceVersion, Reason: personfacts.EvidenceStatusSourceReimported, Supported: true}})
	input.ResolvedAt = time.Now().UTC()
	_, err = s.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	requirements.NoError(err)
	assertions.Empty(policyFactValues(t, s, person))
	after, err := s.LoadPersonSemanticDocumentContext(t.Context(), person)
	requirements.NoError(err)
	assertions.Equal(before.Revision, after.Revision, "ineligible reimport does not change eligible person inputs")
	counts := personFactProjectionCounts(t, s)
	_, err = s.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "policy-room", conversationpolicy.Cold))
	assertions.Equal(counts, personFactProjectionCounts(t, s), "replayed generation and unchanged tier append no events")
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "policy-room", conversationpolicy.Full))
	assertions.Len(policyFactValues(t, s, person), 1)
}

func TestConversationPolicyPeoplePreservesContactsAndManualDecisions(t *testing.T) {
	s, person, source, _, claim := policyFactFixture(t)
	independent := personFactProjectionClaim(person, claim.Target, `"chat"`, "independent-public-contact")
	input := personFactProjectionInput(person, "independent", []personfacts.ProposedClaim{independent}, nil)
	input.ResolvedAt = time.Now().UTC()
	_, err := s.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	require.NoError(t, err)
	require.NoError(t, s.SetConversationPolicyOverride(t.Context(), source, "policy-room", conversationpolicy.Cold))
	assert.Len(t, policyFactValues(t, s, person), 1)
}

func TestConversationPolicyPromotionValidatesActualSource(t *testing.T) {
	for _, mutation := range []string{"deleted", "edited", "unlinked"} {
		t.Run(mutation, func(t *testing.T) {
			requirements := require.New(t)

			s, person, source, message, _ := policyFactFixture(t)
			s.DB().SetMaxOpenConns(1)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			requirements.NoError(s.SetConversationPolicyOverride(ctx, source, "policy-room", conversationpolicy.Cold))
			var err error
			switch mutation {
			case "deleted":
				_, err = s.db.Exec(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`, message)
			case "edited":
				_, err = s.db.Exec(`UPDATE message_bodies SET body_text='A different statement.' WHERE message_id=?`, message)
			case "unlinked":
				_, err = s.db.Exec(`DELETE FROM person_participants WHERE person_id=?`, person)
			}
			requirements.NoError(err)
			requirements.NoError(s.SetConversationPolicyOverride(ctx, source, "policy-room", conversationpolicy.Full))
			assert.Empty(t, policyFactValues(t, s, person))
		})
	}
}

func TestConversationPolicyPeoplePreservesPinnedDecision(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, person, source, _, claim := policyFactFixture(t)
	_, err := s.SetPersonFactPinContext(t.Context(), person, personfacts.TargetRef{Kind: claim.Target.Kind, Key: claim.Target.Key, Revision: claim.Target.Revision}, true, "synthetic-user")
	requirements.NoError(err)
	before, err := s.ListPersonFactPinsContext(t.Context(), person)
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "policy-room", conversationpolicy.Cold))
	assertions.Len(policyFactValues(t, s, person), 1)
	after, err := s.ListPersonFactPinsContext(t.Context(), person)
	requirements.NoError(err)
	assertions.Equal(before, after)
}

func TestConversationPolicyPeopleStatusMigrationPreservesHistory(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, person, source, _, _ := policyFactFixture(t)
	evidence, err := s.ListPersonFactEvidenceContext(t.Context(), person, personfacts.EvidenceFilter{})
	requirements.NoError(err)
	requirements.Len(evidence, 1)
	input := personFactProjectionInput(person, "migration-existing-status", nil, []personfacts.EvidenceStatusChange{{EvidenceKey: evidence[0].Key, SourceVersion: evidence[0].Input.SourceVersion, Reason: personfacts.EvidenceStatusSourceReimported, Supported: true}})
	input.ResolvedAt = time.Now().UTC()
	_, err = s.ApplyPersonFactGenerationContext(t.Context(), input, nil)
	requirements.NoError(err)
	history, err := s.ListPersonFactEvidenceStatusEventsContext(t.Context(), person, personfacts.EvidenceStatusFilter{})
	requirements.NoError(err)
	requirements.Len(history, 1)
	values := policyFactValues(t, s, person)
	oldCheck := `CHECK (reason IN ('source-deleted', 'source-edited', 'scope-unlinked', 'identity-reassigned', 'source-reimported', 'scope-relinked'))`
	if s.IsPostgreSQL() {
		_, err = s.db.Exec(`ALTER TABLE person_fact_evidence_status_events DROP CONSTRAINT person_fact_evidence_status_events_reason_check`)
		requirements.NoError(err)
		_, err = s.db.Exec(`ALTER TABLE person_fact_evidence_status_events ADD CONSTRAINT person_fact_evidence_status_events_reason_check ` + oldCheck)
		requirements.NoError(err)
	} else {
		var ddl string
		requirements.NoError(s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='person_fact_evidence_status_events'`).Scan(&ddl))
		start := strings.Index(ddl, "CHECK (reason IN (")
		requirements.GreaterOrEqual(start, 0)
		end := strings.Index(ddl[start:], "))") + start + 2
		ddl = ddl[:start] + oldCheck + ddl[end:]
		ddl = strings.Replace(ddl, "person_fact_evidence_status_events", "policy_status_old_fixture", 1)
		tx, err := s.db.BeginTx(t.Context(), nil)
		requirements.NoError(err)
		defer func() { _ = tx.Rollback() }()
		for _, statement := range []string{ddl, `INSERT INTO policy_status_old_fixture SELECT * FROM person_fact_evidence_status_events`, `DROP TABLE person_fact_evidence_status_events`, `ALTER TABLE policy_status_old_fixture RENAME TO person_fact_evidence_status_events`, `CREATE INDEX idx_person_fact_evidence_status_latest ON person_fact_evidence_status_events(person_id,evidence_key,source_version,id DESC)`} {
			_, err = tx.ExecContext(t.Context(), statement)
			requirements.NoError(err)
		}
		requirements.NoError(tx.Commit())
	}
	_, err = s.db.Exec(`UPDATE person_fact_evidence_status_events SET reason='conversation-cold' WHERE id=?`, history[0].ID)
	requirements.Error(err, "the actual legacy constraint must reject policy reasons")
	_, err = s.db.Exec(`DELETE FROM applied_migrations WHERE name='person_fact_conversation_policy_status_v1'`)
	requirements.NoError(err)
	_, err = s.db.Exec(`UPDATE archive_metadata SET value='12' WHERE key='schema_version'`)
	requirements.NoError(err)
	requirements.NoError(s.InitSchemaContext(t.Context()))
	after, err := s.ListPersonFactEvidenceStatusEventsContext(t.Context(), person, personfacts.EvidenceStatusFilter{})
	requirements.NoError(err)
	assertions.Equal(history, after)
	assertions.Equal(values, policyFactValues(t, s, person))
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "policy-room", conversationpolicy.Cold))
	assertions.Empty(policyFactValues(t, s, person))
	after, err = s.ListPersonFactEvidenceStatusEventsContext(t.Context(), person, personfacts.EvidenceStatusFilter{})
	requirements.NoError(err)
	assertions.Len(after, 2)
	if !s.IsPostgreSQL() {
		rows, err := s.db.Query(`PRAGMA foreign_key_check`)
		requirements.NoError(err)
		assertions.False(rows.Next())
		requirements.NoError(rows.Close())
	}
}

func TestConversationPolicyPeopleUntrackedProjectionRetracts(t *testing.T) {
	s, person, source, _, _ := policyFactFixture(t)
	_, err := s.SetPersonTrackingContext(t.Context(), person, false)
	require.NoError(t, err)
	require.NoError(t, s.SetConversationPolicyOverride(t.Context(), source, "policy-room", conversationpolicy.Cold))
	assert.Empty(t, policyFactValues(t, s, person))
}

func TestConversationPolicyPeopleBriefInvalidation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, person, source, _, _ := policyFactFixture(t)
	_, err := s.db.Exec(`INSERT INTO person_briefs(person_id,version,generation_id,status,program_id,program_version,program_fingerprint,provider,provider_version,model,model_version,provider_policy_fingerprint,boundary_json,structured_json,rendered_text,renderer_policy,generated_at) SELECT person_id,1,id,'current',program_id,program_version,program_fingerprint,provider,provider_version,model,model_version,provider_policy_fingerprint,'{}','{}','Synthetic mixed-input brief','test/v1',resolved_at FROM person_fact_generations WHERE person_id=? ORDER BY id DESC LIMIT 1`, person)
	requirements.NoError(err)
	before, err := s.GetPersonBriefContext(t.Context(), person, 0)
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "policy-room", conversationpolicy.Cold))
	_, err = s.GetPersonBriefContext(t.Context(), person, 0)
	requirements.ErrorIs(err, ErrPersonBriefNotFound)
	historical, err := s.GetPersonBriefContext(t.Context(), person, before.Version)
	requirements.NoError(err)
	assertions.Equal(PersonBriefStatusSuperseded, historical.Status)
	assertions.Equal(before.RenderedText, historical.RenderedText)
}

func TestConversationPolicyPeoplePreservesIndependentFullEvidence(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s, person, source, message, claim := policyFactFixture(t)
	room, err := s.EnsureConversation(source, "independent-full", "Independent Full")
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "independent-full", conversationpolicy.Full))
	var sender int64
	requirements.NoError(s.db.QueryRow(`SELECT sender_id FROM messages WHERE id=?`, message).Scan(&sender))
	id, err := s.UpsertMessage(&Message{SourceID: source, SourceMessageID: "independent-message", ConversationID: room, MessageType: "chat", SenderID: sql.NullInt64{Int64: sender, Valid: true}, SentAt: sql.NullTime{Time: time.Now().UTC().Truncate(time.Second), Valid: true}})
	requirements.NoError(err)
	_, err = s.db.Exec(`INSERT INTO message_bodies(message_id,body_text) VALUES(?,?)`, id, "I prefer chat independently.")
	requirements.NoError(err)
	seeds, _, err := s.hydratePersonSweepMessageSet(t.Context(), person, peoplesweep.SourceConversationText, []int64{id}, 0)
	requirements.NoError(err)
	requirements.Len(seeds, 1)
	evidence, err := peoplesweep.PersonFactEvidenceInput(seeds[0])
	requirements.NoError(err)
	claim.Evidence = []personfacts.EvidenceInput{evidence}
	input := personFactProjectionInput(person, "independent-full", []personfacts.ProposedClaim{claim}, nil)
	input.ResolvedAt = time.Now().UTC()
	_, err = s.ApplyPersonFactGenerationContext(t.Context(), input, PersonSweepEvidenceAligner{Store: s})
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "policy-room", conversationpolicy.Cold))
	assertions.Len(policyFactValues(t, s, person), 1)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source, "independent-full", conversationpolicy.Cold))
	assertions.Empty(policyFactValues(t, s, person))
}
