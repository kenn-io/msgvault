package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/novelty"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

func sweepSelectionRecord(target string, messageID int64) peoplesweep.SelectionRecord {
	purpose := peoplesweep.SelectionPurposeExtractionContext
	mode := novelty.ModeNovelty
	lane := novelty.LaneNovelty
	score := new(int(8000))
	if target == "" {
		purpose = peoplesweep.SelectionPurposeBrief
		mode = novelty.ModeRecency
		lane = novelty.LaneRecency
		score = nil
	}
	return peoplesweep.SelectionRecord{Purpose: purpose, TargetKey: target, PolicyVersion: novelty.PolicyVersion, Mode: mode,
		ParamsFingerprint: strings.Repeat("a", 64), VectorGenerationID: new(int64(1)), VectorFingerprint: "vector-generation",
		AnchorSetSHA256: strings.Repeat("b", 64), PoolCount: 5, ScoredCount: 4, AnchorCount: 2, AnchorScoredCount: 1,
		SelectionSHA256: strings.Repeat("c", 64), ElapsedMicroseconds: 25,
		Items: []peoplesweep.SelectionItem{{Ordinal: 0, MessageID: messageID, EvidenceID: "evidence-1", Lane: lane, BatchOrdinal: new(int(0)), NoveltyBP: new(int(10000)), SupportBP: new(int(6000)), ScoreBP: score}},
	}
}

func TestStartPersonSweepAttemptPersistsSelections(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonSweepBudgetFixture(t, "selection-roundtrip")
	request := sweepStartAttempt(t, "attempt-selection-records", f.runID, f.personID, 2)
	brief, extraction := sweepSelectionRecord("", 2), sweepSelectionRecord("employer", 1<<40)
	brief.VectorGenerationID = nil
	brief.VectorFingerprint = ""
	brief.AnchorSetSHA256 = ""
	brief.ScoredCount, brief.AnchorCount, brief.AnchorScoredCount = 0, 0, 0
	extraction.VectorGenerationID = new(int64(1 << 41))
	extraction.ElapsedMicroseconds = 1 << 35
	brief.Items = append(brief.Items, peoplesweep.SelectionItem{Ordinal: 1, MessageID: 4, EvidenceID: "mandatory", Lane: novelty.LaneMandatory, BatchOrdinal: new(int(0))})
	extraction.Items = append(extraction.Items, peoplesweep.SelectionItem{Ordinal: 1, MessageID: 3, EvidenceID: "trimmed", Lane: novelty.LaneFill})
	request.Selections = []peoplesweep.SelectionRecord{brief, extraction}
	require.NoError(f.store.StartPersonSweepAttempt(t.Context(), request))
	got, err := f.store.ListPersonSweepSelections(t.Context(), request.ID)
	require.NoError(err)
	assert.Equal(request.Selections, got)
	var selected, recency, novel, fill int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT selected_count,recency_count,novelty_count,fill_count FROM person_sweep_selections WHERE attempt_id=? AND purpose='extraction_context'`), request.ID).Scan(&selected, &recency, &novel, &fill))
	assert.Equal(2, selected)
	assert.Zero(recency)
	assert.Equal(1, novel)
	assert.Equal(1, fill)
}

func TestStartPersonSweepAttemptRejectsInvalidSelectionAtomically(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid", true: "duplicate"}[duplicate], func(t *testing.T) {
			f := newPersonSweepBudgetFixture(t, "selection-atomic")
			request := sweepStartAttempt(t, "attempt-invalid-selection", f.runID, f.personID, 2)
			request.Selections = []peoplesweep.SelectionRecord{sweepSelectionRecord("employer", 1)}
			if duplicate {
				request.Selections = append(request.Selections, request.Selections[0])
			} else {
				request.Selections[0].Items[0].MessageID = 0
			}
			require.Error(t, f.store.StartPersonSweepAttempt(t.Context(), request))
			var count int
			require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT COUNT(*) FROM person_sweep_attempts WHERE id=?`), request.ID).Scan(&count))
			assert.Zero(t, count)
		})
	}
}

func TestStartPersonSweepAttemptWithoutSelectionsWritesNone(t *testing.T) {
	f := newPersonSweepBudgetFixture(t, "selection-empty")
	got, err := f.store.ListPersonSweepSelections(t.Context(), f.attemptID)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestRecordPersonSweepSelectionsReplacesTarget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonSweepBudgetFixture(t, "selection-replace")
	employer, city := sweepSelectionRecord("employer", 1), sweepSelectionRecord("city", 2)
	require.NoError(f.store.RecordPersonSweepSelections(t.Context(), f.attemptID, []peoplesweep.SelectionRecord{employer, city}))
	replacement := sweepSelectionRecord("employer", 3)
	replacement.Items[0].EvidenceID = "replacement"
	require.NoError(f.store.RecordPersonSweepSelections(t.Context(), f.attemptID, []peoplesweep.SelectionRecord{replacement}))
	got, err := f.store.ListPersonSweepSelections(t.Context(), f.attemptID)
	require.NoError(err)
	assert.Equal([]peoplesweep.SelectionRecord{city, replacement}, got)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_sweep_attempts SET status='succeeded' WHERE id=?`), f.attemptID)
	require.NoError(err)
	assert.Error(f.store.RecordPersonSweepSelections(t.Context(), f.attemptID, []peoplesweep.SelectionRecord{employer}))
}

func reserveSelectionBatch(t *testing.T, f personSweepBudgetFixture, ordinal int) peoplesweep.BudgetReservation {
	t.Helper()
	reservation, err := f.store.ReservePersonSweepBudget(t.Context(), sweepReservation(f, ordinal, 100, "provider-fingerprint", generousSweepBudget()))
	require.NoError(t, err)
	return reservation
}

func noveltySelectionItem(ordinal int, messageID int64, batch *int) peoplesweep.SelectionItem {
	return peoplesweep.SelectionItem{Ordinal: ordinal, MessageID: messageID, EvidenceID: strings.Repeat("e", ordinal+1), Lane: novelty.LaneNovelty, BatchOrdinal: batch, ScoreBP: new(int(8000))}
}

func TestRecentNoveltyPicksCountOnlyDispatchedSentPicks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonSweepBudgetFixture(t, "selection-dispatch")
	extraction := sweepSelectionRecord("employer", 1)
	recency := noveltySelectionItem(2, 3, new(int(0)))
	recency.Lane = novelty.LaneRecency
	recency.ScoreBP = nil
	extraction.Items = []peoplesweep.SelectionItem{
		noveltySelectionItem(0, 1, new(int(0))),
		noveltySelectionItem(1, 2, nil), recency,
		noveltySelectionItem(3, 4, new(int(1))),
	}
	brief := sweepSelectionRecord("", 5)
	brief.Mode = novelty.ModeNovelty
	brief.Items = []peoplesweep.SelectionItem{noveltySelectionItem(0, 5, new(int(0)))}
	require.NoError(f.store.RecordPersonSweepSelections(t.Context(), f.attemptID, []peoplesweep.SelectionRecord{extraction, brief}))
	first := reserveSelectionBatch(t, f, 0)
	require.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), first, sweepAttemptLease(f)))
	cancelled := reserveSelectionBatch(t, f, 1)
	require.NoError(f.store.ReleasePersonSweepBudget(t.Context(), cancelled))
	got, err := f.store.ListRecentNoveltyPicks(t.Context(), f.personID, sweepBudgetNow().Add(-time.Hour))
	require.NoError(err)
	assert.Equal(map[int64]struct{}{1: {}}, got)
	got, err = f.store.ListRecentNoveltyPicks(t.Context(), f.personID+1, sweepBudgetNow().Add(-time.Hour))
	require.NoError(err)
	assert.Empty(got)
	got, err = f.store.ListRecentNoveltyPicks(t.Context(), f.personID, sweepBudgetNow().Add(time.Hour))
	require.NoError(err)
	assert.Empty(got)
}

func finalizeSelectionFailure(t *testing.T, f personSweepBudgetFixture, reservations ...peoplesweep.BudgetReservation) {
	t.Helper()
	require.NoError(t, f.store.FinalizePersonSweepFailure(t.Context(), peoplesweep.FailureFinalization{Lease: sweepAttemptLease(f), AttemptID: f.attemptID, Class: peoplesweep.FailureProviderHTTP, RetryAt: sweepBudgetNow().Add(time.Hour), Reservations: reservations, FinalizedAt: sweepBudgetNow().Add(time.Minute)}))
}

func TestRecentNoveltyPicksCountFailureAfterStart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonSweepBudgetFixture(t, "selection-failed")
	require.NoError(f.store.RecordPersonSweepSelections(t.Context(), f.attemptID, []peoplesweep.SelectionRecord{sweepSelectionRecord("employer", 1)}))
	first := reserveSelectionBatch(t, f, 0)
	require.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), first, sweepAttemptLease(f)))
	finalizeSelectionFailure(t, f, first)
	var status string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT status FROM person_sweep_batches WHERE attempt_id=? AND batch_ordinal=0`), f.attemptID).Scan(&status))
	assert.Equal("failed", status)
	got, err := f.store.ListRecentNoveltyPicks(t.Context(), f.personID, sweepBudgetNow().Add(-time.Hour))
	require.NoError(err)
	assert.Equal(map[int64]struct{}{1: {}}, got)
}

func TestRecentNoveltyPicksMixedDispatchedAndCancelledBatchesDuringFailureFinalization(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonSweepBudgetFixture(t, "selection-mixed-finalized")
	record := sweepSelectionRecord("employer", 1)
	record.Items = []peoplesweep.SelectionItem{
		noveltySelectionItem(0, 1, new(int(0))),
		noveltySelectionItem(1, 2, new(int(1))),
	}
	require.NoError(f.store.RecordPersonSweepSelections(t.Context(), f.attemptID, []peoplesweep.SelectionRecord{record}))
	first, second := reserveSelectionBatch(t, f, 0), reserveSelectionBatch(t, f, 1)
	require.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), first, sweepAttemptLease(f)))
	finalizeSelectionFailure(t, f, first, second)
	var status string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT status FROM person_sweep_batches WHERE attempt_id=? AND batch_ordinal=1`), f.attemptID).Scan(&status))
	assert.Equal("cancelled", status)
	got, err := f.store.ListRecentNoveltyPicks(t.Context(), f.personID, sweepBudgetNow().Add(-time.Hour))
	require.NoError(err)
	assert.Equal(map[int64]struct{}{1: {}}, got)
}

func insertSelectionSchemaRecord(t *testing.T, f personSweepBudgetFixture, purpose, target, mode string) error {
	t.Helper()
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_sweep_selections
 (attempt_id,purpose,target_key,policy_version,mode,fallback_reason,params_fingerprint,
 pool_count,scored_count,anchor_count,anchor_scored_count,selected_count,recency_count,novelty_count,fill_count,selection_sha256)
 VALUES (?,?,?,'novelty-v1',?,'',?,1,1,0,0,1,0,1,0,?)`), f.attemptID, purpose, target, mode, strings.Repeat("a", 64), strings.Repeat("b", 64))
	return err
}

func insertSelectionSchemaItem(t *testing.T, f personSweepBudgetFixture, purpose, target, lane string, sent int, batch, score any) error {
	t.Helper()
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_sweep_selection_items
 (attempt_id,purpose,target_key,ordinal,message_id,evidence_id,lane,sent,batch_ordinal,score_bp)
 VALUES (?,?,?,0,1,'schema-evidence',?,?,?,?)`), f.attemptID, purpose, target, lane, sent, batch, score)
	return err
}

func TestPersonSweepSelectionSchemaChecks(t *testing.T) {
	for _, tc := range []struct {
		name, purpose, target, lane string
		sent                        int
		batch, score                any
	}{
		{"novelty requires score", "extraction_context", "employer", "novelty", 1, 0, nil},
		{"sent requires batch", "extraction_context", "employer", "novelty", 1, nil, 8000},
		{"unsent forbids batch", "extraction_context", "employer", "novelty", 0, 0, 8000},
		{"mandatory extraction", "extraction_context", "employer", "mandatory", 1, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPersonSweepBudgetFixture(t, "selection-schema")
			require.NoError(t, insertSelectionSchemaRecord(t, f, tc.purpose, tc.target, "recency"))
			assert.Error(t, insertSelectionSchemaItem(t, f, tc.purpose, tc.target, tc.lane, tc.sent, tc.batch, tc.score))
		})
	}
	t.Run("fallback reason", func(t *testing.T) {
		f := newPersonSweepBudgetFixture(t, "selection-schema-reason")
		assert.Error(t, insertSelectionSchemaRecord(t, f, "extraction_context", "employer", "recency_fallback"))
	})
	t.Run("brief target", func(t *testing.T) {
		f := newPersonSweepBudgetFixture(t, "selection-schema-target")
		assert.Error(t, insertSelectionSchemaRecord(t, f, "brief", "employer", "recency"))
	})
	t.Run("valid and cascades", func(t *testing.T) {
		require := require.New(t)
		f := newPersonSweepBudgetFixture(t, "selection-schema-cascade")
		require.NoError(insertSelectionSchemaRecord(t, f, "extraction_context", "employer", "recency"))
		require.NoError(insertSelectionSchemaItem(t, f, "extraction_context", "employer", "novelty", 1, 0, 8000))
		_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`DELETE FROM person_sweep_attempts WHERE id=?`), f.attemptID)
		require.NoError(err)
		for _, table := range []string{"person_sweep_selections", "person_sweep_selection_items"} {
			var count int
			require.NoError(f.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&count))
			assert.Zero(t, count)
		}
	})
}

func TestRecentNoveltyPicksMatchCallPurpose(t *testing.T) {
	for _, purpose := range []string{peoplesweep.ProviderCallPurposePrimary, peoplesweep.ProviderCallPurposeBrief} {
		t.Run(purpose, func(t *testing.T) {
			require := require.New(t)
			f := newPersonSweepBudgetFixture(t, "selection-call-purpose")
			extraction := sweepSelectionRecord("employer", 1)
			brief := sweepSelectionRecord("", 2)
			brief.Mode = novelty.ModeNovelty
			brief.Items = []peoplesweep.SelectionItem{noveltySelectionItem(0, 2, new(int(0)))}
			require.NoError(f.store.RecordPersonSweepSelections(t.Context(), f.attemptID, []peoplesweep.SelectionRecord{extraction, brief}))
			request := sweepReservation(f, 0, 100, "provider-fingerprint", generousSweepBudget())
			request.Purpose = purpose
			reservation, err := f.store.ReservePersonSweepBudget(t.Context(), request)
			require.NoError(err)
			require.NoError(f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation, sweepAttemptLease(f)))
			got, err := f.store.ListRecentNoveltyPicks(t.Context(), f.personID, sweepBudgetNow().Add(-time.Hour))
			require.NoError(err)
			want := int64(1)
			if purpose == peoplesweep.ProviderCallPurposeBrief {
				want = 2
			}
			assert.Equal(t, map[int64]struct{}{want: {}}, got)
		})
	}
}
