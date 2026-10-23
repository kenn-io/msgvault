package peoplesweep

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/novelty"
)

func validSelectionRecord() SelectionRecord {
	return SelectionRecord{
		Purpose: SelectionPurposeExtractionContext, TargetKey: "employer", PolicyVersion: novelty.PolicyVersion,
		Mode: novelty.ModeNovelty, ParamsFingerprint: strings.Repeat("a", 64), VectorGenerationID: new(int64(1)),
		VectorFingerprint: "vector-generation", AnchorSetSHA256: strings.Repeat("b", 64),
		PoolCount: 2, ScoredCount: 2, AnchorCount: 1, AnchorScoredCount: 1,
		SelectionSHA256: strings.Repeat("c", 64), ElapsedMicroseconds: 10,
		Items: []SelectionItem{{Ordinal: 0, MessageID: 1, EvidenceID: "evidence-1", Lane: novelty.LaneNovelty, BatchOrdinal: new(int(0)), NoveltyBP: new(int(9000)), SupportBP: new(int(7000)), ScoreBP: new(int(7650))}},
	}
}

func TestSelectionRecordValidate(t *testing.T) {
	for _, mode := range []novelty.Mode{novelty.ModeRecency, novelty.ModeNovelty, novelty.ModeNoveltyInSample, novelty.ModeRecencyFallback} {
		t.Run(string(mode), func(t *testing.T) {
			record := validSelectionRecord()
			record.Mode = mode
			if mode == novelty.ModeRecencyFallback {
				record.FallbackReason = novelty.FallbackLowCoverage
			}
			require.NoError(t, record.Validate())
			assert.True(t, record.Items[0].Sent())
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*SelectionRecord)
	}{
		{"purpose", func(r *SelectionRecord) { r.Purpose = "unknown" }},
		{"extraction target", func(r *SelectionRecord) { r.TargetKey = "" }},
		{"brief target", func(r *SelectionRecord) { r.Purpose = SelectionPurposeBrief }},
		{"mode", func(r *SelectionRecord) { r.Mode = "unknown" }},
		{"reason vocabulary", func(r *SelectionRecord) { r.Mode = novelty.ModeRecencyFallback; r.FallbackReason = "unknown" }},
		{"fallback reason required", func(r *SelectionRecord) { r.Mode = novelty.ModeRecencyFallback }},
		{"reason only on fallback", func(r *SelectionRecord) { r.FallbackReason = novelty.FallbackLowCoverage }},
		{"params short", func(r *SelectionRecord) { r.ParamsFingerprint = "abc" }},
		{"params uppercase", func(r *SelectionRecord) { r.ParamsFingerprint = strings.Repeat("A", 64) }},
		{"selection hash", func(r *SelectionRecord) { r.SelectionSHA256 = "invalid" }},
		{"anchor hash", func(r *SelectionRecord) { r.AnchorSetSHA256 = "invalid" }},
		{"pool negative", func(r *SelectionRecord) { r.PoolCount = -1 }},
		{"scored negative", func(r *SelectionRecord) { r.ScoredCount = -1 }},
		{"anchors negative", func(r *SelectionRecord) { r.AnchorCount = -1 }},
		{"anchor scored negative", func(r *SelectionRecord) { r.AnchorScoredCount = -1 }},
		{"scored exceeds pool", func(r *SelectionRecord) { r.ScoredCount = 3 }},
		{"scored anchors exceeds pool", func(r *SelectionRecord) { r.AnchorScoredCount = 2 }},
		{"elapsed negative", func(r *SelectionRecord) { r.ElapsedMicroseconds = -1 }},
		{"ordinal negative", func(r *SelectionRecord) { r.Items[0].Ordinal = -1 }},
		{"ordinal gap", func(r *SelectionRecord) { r.Items[0].Ordinal = 1 }},
		{"message positive", func(r *SelectionRecord) { r.Items[0].MessageID = 0 }},
		{"evidence required", func(r *SelectionRecord) { r.Items[0].EvidenceID = "" }},
		{"evidence unique", func(r *SelectionRecord) { i := r.Items[0]; i.Ordinal = 1; r.Items = append(r.Items, i) }},
		{"lane vocabulary", func(r *SelectionRecord) { r.Items[0].Lane = "unknown" }},
		{"mandatory only in brief", func(r *SelectionRecord) { r.Items[0].Lane = novelty.LaneMandatory; r.Items[0].ScoreBP = nil }},
		{"novelty score required", func(r *SelectionRecord) { r.Items[0].ScoreBP = nil }},
		{"score only for novelty", func(r *SelectionRecord) { r.Items[0].Lane = novelty.LaneRecency }},
		{"batch nonnegative", func(r *SelectionRecord) { r.Items[0].BatchOrdinal = new(int(-1)) }},
		{"novelty bp low", func(r *SelectionRecord) { r.Items[0].NoveltyBP = new(int(-1)) }},
		{"novelty bp high", func(r *SelectionRecord) { r.Items[0].NoveltyBP = new(int(10001)) }},
		{"support bp low", func(r *SelectionRecord) { r.Items[0].SupportBP = new(int(-1)) }},
		{"support bp high", func(r *SelectionRecord) { r.Items[0].SupportBP = new(int(10001)) }},
		{"score bp low", func(r *SelectionRecord) { r.Items[0].ScoreBP = new(int(-1)) }},
		{"score bp high", func(r *SelectionRecord) { r.Items[0].ScoreBP = new(int(10001)) }},
		{"novelty generation", func(r *SelectionRecord) { r.VectorGenerationID = nil }},
		{"in sample generation", func(r *SelectionRecord) { r.Mode = novelty.ModeNoveltyInSample; r.VectorGenerationID = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := validSelectionRecord()
			tc.change(&record)
			assert.Error(t, record.Validate())
		})
	}
	t.Run("optional values", func(t *testing.T) {
		record := validSelectionRecord()
		record.Mode = novelty.ModeRecency
		record.Purpose = SelectionPurposeBrief
		record.TargetKey = ""
		record.AnchorSetSHA256 = ""
		record.VectorGenerationID = nil
		record.Items[0].Lane = novelty.LaneMandatory
		record.Items[0].ScoreBP = nil
		record.Items[0].BatchOrdinal = nil
		record.Items[0].NoveltyBP = nil
		record.Items[0].SupportBP = nil
		require.NoError(t, record.Validate())
		assert.False(t, record.Items[0].Sent())
	})
}

func TestSelectionFingerprint(t *testing.T) {
	assert := assert.New(t)
	assert.Empty(SelectionFingerprint(nil))
	recency := validSelectionRecord()
	recency.Mode = novelty.ModeRecency
	assert.Empty(SelectionFingerprint([]SelectionRecord{recency}))
	fallback := validSelectionRecord()
	fallback.Mode = novelty.ModeRecencyFallback
	fallback.FallbackReason = novelty.FallbackLowCoverage
	assert.NotEmpty(SelectionFingerprint([]SelectionRecord{fallback}))
	brief := validSelectionRecord()
	brief.Purpose = SelectionPurposeBrief
	brief.TargetKey = ""
	brief.Mode = novelty.ModeRecency
	records := []SelectionRecord{fallback, brief}
	assert.Equal("42f584d1dee8ff684d06725cb6980dc088a5bee3efb5e5a7818e2ac4b1550985", SelectionFingerprint(records))
	assert.Equal(SelectionFingerprint(records), SelectionFingerprint([]SelectionRecord{brief, fallback}))
	assert.Equal(SelectionPurposeExtractionContext, records[0].Purpose)
}

func TestSelectionFingerprintRepeatedIdentityIsOrderIndependent(t *testing.T) {
	first := validSelectionRecord()
	second := validSelectionRecord()
	second.SelectionSHA256 = strings.Repeat("d", 64)
	got := SelectionFingerprint([]SelectionRecord{first, second})
	assert.NotEmpty(t, got)
	assert.Equal(t, got, SelectionFingerprint([]SelectionRecord{second, first}))
}
