package peoplesweep

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/novelty"
)

// SelectionPurpose identifies the provider packet a selection contributes to.
type SelectionPurpose string

const (
	SelectionPurposeExtractionContext SelectionPurpose = "extraction_context"
	SelectionPurposeBrief             SelectionPurpose = "brief"
)

// SelectionItem records a pick's priority and the call whose packet carried it.
// A nil BatchOrdinal means the pick was trimmed before dispatch.
type SelectionItem struct {
	Ordinal      int
	MessageID    int64
	EvidenceID   string
	Lane         novelty.Lane
	BatchOrdinal *int
	NoveltyBP    *int
	SupportBP    *int
	ScoreBP      *int
}

// Sent reports whether the item belongs to a provider call packet.
func (i SelectionItem) Sent() bool { return i.BatchOrdinal != nil }

// SelectionRecord holds identifiers and selection provenance, never excerpts.
// Lane and selected counts are derived from Items when the record is stored.
type SelectionRecord struct {
	Purpose             SelectionPurpose
	TargetKey           string
	PolicyVersion       string
	Mode                novelty.Mode
	FallbackReason      novelty.FallbackReason
	ParamsFingerprint   string
	VectorGenerationID  *int64
	VectorFingerprint   string
	AnchorSetSHA256     string
	PoolCount           int
	ScoredCount         int
	AnchorCount         int
	AnchorScoredCount   int
	SelectionSHA256     string
	ElapsedMicroseconds int64
	Items               []SelectionItem
}

var selectionSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate checks the schema contract and ordered pick identities.
func (r SelectionRecord) Validate() error {
	if r.Purpose != SelectionPurposeExtractionContext && r.Purpose != SelectionPurposeBrief {
		return fmt.Errorf("selection: invalid purpose %q", r.Purpose)
	}
	if (r.Purpose == SelectionPurposeBrief) != (r.TargetKey == "") {
		return errors.New("selection: only briefs must have an empty target key")
	}
	switch r.Mode {
	case novelty.ModeRecency, novelty.ModeNovelty, novelty.ModeNoveltyInSample, novelty.ModeRecencyFallback:
	default:
		return fmt.Errorf("selection: invalid mode %q", r.Mode)
	}
	switch r.FallbackReason {
	case "", novelty.FallbackVectorDisabled, novelty.FallbackNoActiveGeneration,
		novelty.FallbackIndexStale, novelty.FallbackCapabilityMissing, novelty.FallbackLowCoverage,
		novelty.FallbackVectorLoadFailed, novelty.FallbackWorkLimit:
	default:
		return fmt.Errorf("selection: invalid fallback reason %q", r.FallbackReason)
	}
	if (r.Mode == novelty.ModeRecencyFallback) != (r.FallbackReason != "") {
		return errors.New("selection: a fallback reason is required exactly in recency_fallback mode")
	}
	if (r.Mode == novelty.ModeNovelty || r.Mode == novelty.ModeNoveltyInSample) && r.VectorGenerationID == nil {
		return errors.New("selection: novelty requires a vector generation")
	}
	if !selectionSHA256Pattern.MatchString(r.ParamsFingerprint) || !selectionSHA256Pattern.MatchString(r.SelectionSHA256) ||
		(r.AnchorSetSHA256 != "" && !selectionSHA256Pattern.MatchString(r.AnchorSetSHA256)) {
		return errors.New("selection: fingerprints must be lowercase 64-hex SHA-256")
	}
	if r.PoolCount < 0 || r.ScoredCount < 0 || r.AnchorCount < 0 || r.AnchorScoredCount < 0 ||
		r.ScoredCount > r.PoolCount || r.AnchorScoredCount > r.AnchorCount || r.ElapsedMicroseconds < 0 {
		return errors.New("selection: invalid counts or elapsed time")
	}
	seen := make(map[string]struct{}, len(r.Items))
	for ordinal, item := range r.Items {
		if item.Ordinal != ordinal || item.MessageID <= 0 || item.EvidenceID == "" {
			return fmt.Errorf("selection item %d: contiguous ordinal, positive message ID and evidence ID are required", ordinal)
		}
		if _, exists := seen[item.EvidenceID]; exists {
			return fmt.Errorf("selection item %d: duplicate evidence ID %q", ordinal, item.EvidenceID)
		}
		seen[item.EvidenceID] = struct{}{}
		switch item.Lane {
		case novelty.LaneMandatory:
			if r.Purpose != SelectionPurposeBrief {
				return fmt.Errorf("selection item %d: mandatory picks require a brief", ordinal)
			}
		case novelty.LaneRecency, novelty.LaneNovelty, novelty.LaneFill:
		default:
			return fmt.Errorf("selection item %d: invalid lane %q", ordinal, item.Lane)
		}
		if (item.Lane == novelty.LaneNovelty) != (item.ScoreBP != nil) {
			return fmt.Errorf("selection item %d: score is required exactly for novelty picks", ordinal)
		}
		if item.BatchOrdinal != nil && *item.BatchOrdinal < 0 {
			return fmt.Errorf("selection item %d: batch ordinal must be nonnegative", ordinal)
		}
		for _, bp := range []*int{item.NoveltyBP, item.SupportBP, item.ScoreBP} {
			if bp != nil && (*bp < 0 || *bp > 10000) {
				return fmt.Errorf("selection item %d: basis points must be between 0 and 10000", ordinal)
			}
		}
	}
	return nil
}

// SelectionFingerprint binds generations to their selection policy and picks.
// Configured recency preserves legacy generation identities. Record order does
// not matter; triples are sorted by purpose, target key and selection digest.
// Persistence requires unique purpose/target identities.
func SelectionFingerprint(records []SelectionRecord) string {
	triples := make([][3]string, 0, len(records))
	configuredRecency := true
	for _, record := range records {
		if record.Mode != novelty.ModeRecency {
			configuredRecency = false
		}
		triples = append(triples, [3]string{string(record.Purpose), record.TargetKey, record.SelectionSHA256})
	}
	if configuredRecency {
		return ""
	}
	slices.SortStableFunc(triples, func(a, b [3]string) int {
		if order := strings.Compare(a[0], b[0]); order != 0 {
			return order
		}
		if order := strings.Compare(a[1], b[1]); order != 0 {
			return order
		}
		return strings.Compare(a[2], b[2])
	})
	// Only string triples are encoded, so replacing invalid UTF-8 makes this
	// operation total even before the records reach database validation.
	encoded, _ := json.Marshal(triples, json.Deterministic(true), jsontext.AllowInvalidUTF8(true))
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
