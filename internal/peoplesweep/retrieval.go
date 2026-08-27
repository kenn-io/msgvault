package peoplesweep

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

type ContextArchive interface {
	ListPersonSweepHistoricalCandidates(ctx context.Context, request HistoricalCandidateRequest) ([]int64, error)
	SearchPersonSweepMessages(ctx context.Context, request ContextRequest) ([]EvidenceItem, error)
	SearchPersonSweepDocuments(ctx context.Context, request DocumentContextRequest) ([]EvidenceItem, error)
}

type ContextRetriever interface {
	RetrievePersonSweepContext(ctx context.Context, request ContextRequest) ([]EvidenceItem, error)
}

type contextRetriever struct {
	archive ContextArchive
}

func NewContextRetriever(archive ContextArchive) ContextRetriever {
	return &contextRetriever{archive: archive}
}

func (r *contextRetriever) RetrievePersonSweepContext(ctx context.Context, request ContextRequest) ([]EvidenceItem, error) {
	if r.archive == nil || request.PersonID <= 0 {
		return nil, errors.New("person sweep context requires archive and person")
	}
	limit := request.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 2000 {
		return nil, errors.New("person sweep context limit exceeds 2000 items")
	}
	candidates := append([]int64(nil), request.CandidateMessageIDs...)
	if len(candidates) == 0 {
		candidateLimit := request.HistoricalCandidateLimit
		if candidateLimit <= 0 {
			candidateLimit = 2000
		}
		candidateLimit = min(candidateLimit, 2000)
		var err error
		candidates, err = r.archive.ListPersonSweepHistoricalCandidates(ctx, HistoricalCandidateRequest{
			PersonID: request.PersonID, SourceClasses: append([]SourceClass(nil), request.SourceClasses...),
			SourceSince: request.SourceSince, SourceUntil: request.SourceUntil, Limit: candidateLimit,
		})
		if err != nil {
			return nil, fmt.Errorf("list person sweep candidates: %w", err)
		}
	}
	if len(candidates) > 2000 {
		return nil, errors.New("person sweep candidate population exceeds 2000 IDs")
	}
	for _, id := range candidates {
		if id <= 0 {
			return nil, errors.New("person sweep candidate IDs must be positive")
		}
	}
	candidates = stableUniquePositive(candidates)
	after, before, err := contextDateBounds(request.SourceSince, request.SourceUntil)
	if err != nil {
		return nil, err
	}
	allowed := make(map[int64]struct{}, len(candidates))
	for _, id := range candidates {
		allowed[id] = struct{}{}
	}
	request.CandidateMessageIDs = candidates
	lexical, err := r.archive.SearchPersonSweepMessages(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("search person sweep messages: %w", err)
	}
	query := contextQuery(request)

	type ranked struct {
		item  EvidenceItem
		score float64
		order int
	}
	byKey := make(map[string]*ranked)
	order := 0
	add := func(item EvidenceItem, rank int) error {
		if item.PersonID != request.PersonID {
			return fmt.Errorf("archive result %d belongs to person %d, not requested person %d",
				item.Ref.MessageID, item.PersonID, request.PersonID)
		}
		if err := ValidatePersonSweepEvidenceItem(item); err != nil {
			return fmt.Errorf("invalid person sweep archive item: %w", err)
		}
		if _, ok := allowed[item.Ref.MessageID]; !ok {
			return fmt.Errorf("archive result %d escaped person candidate scope", item.Ref.MessageID)
		}
		if !contextAllowsItem(item, request.SourceClasses, after, before) {
			return nil
		}
		key := evidenceCoordinateKey(item.Ref)
		entry, ok := byKey[key]
		if !ok {
			entry = &ranked{item: item, order: order}
			byKey[key] = entry
			order++
		}
		entry.score += 1 / float64(60+rank)
		return nil
	}
	seenLexical := make(map[string]struct{}, len(lexical))
	lexicalRank := 0
	for _, item := range lexical {
		if item.PersonID != request.PersonID {
			return nil, fmt.Errorf("archive result %d belongs to person %d, not requested person %d",
				item.Ref.MessageID, item.PersonID, request.PersonID)
		}
		key := evidenceCoordinateKey(item.Ref)
		if _, ok := seenLexical[key]; ok {
			continue
		}
		seenLexical[key] = struct{}{}
		lexicalRank++
		if err := add(item, lexicalRank); err != nil {
			return nil, err
		}
	}
	results := make([]ranked, 0, len(byKey))
	for _, entry := range byKey {
		results = append(results, *entry)
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].order < results[j].order
	})

	var documents []EvidenceItem
	if sweepClassAllowed(request.SourceClasses, SourceDocumentText) {
		documents, err = r.archive.SearchPersonSweepDocuments(ctx, DocumentContextRequest{
			PersonID: request.PersonID, CandidateMessageIDs: candidates,
			Query: query, After: after, Before: before, Limit: limit,
		})
		if err != nil {
			return nil, fmt.Errorf("search person sweep documents: %w", err)
		}
	}
	out := make([]EvidenceItem, 0, min(limit, len(results)+len(documents)))
	seen := make(map[string]struct{})
	for _, entry := range results {
		key := evidenceCoordinateKey(entry.item.Ref)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, entry.item)
		if len(out) == limit {
			return out, nil
		}
	}
	for _, item := range documents {
		if item.PersonID != request.PersonID {
			return nil, fmt.Errorf("document result %d belongs to person %d, not requested person %d",
				item.Ref.MessageID, item.PersonID, request.PersonID)
		}
		if err := ValidatePersonSweepEvidenceItem(item); err != nil {
			return nil, fmt.Errorf("invalid person sweep document item: %w", err)
		}
		if _, ok := allowed[item.Ref.MessageID]; !ok {
			return nil, fmt.Errorf("document result %d escaped person candidate scope", item.Ref.MessageID)
		}
		if !contextAllowsItem(item, request.SourceClasses, after, before) {
			continue
		}
		key := evidenceCoordinateKey(item.Ref)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func stableUniquePositive(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := ids[:0]
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func contextDateBounds(since, until string) (*time.Time, *time.Time, error) {
	var after, before *time.Time
	if since != "" {
		value, err := time.Parse("2006-01-02", since)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid source since date: %w", err)
		}
		value = value.UTC()
		after = &value
	}
	if until != "" {
		value, err := time.Parse("2006-01-02", until)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid source until date: %w", err)
		}
		value = value.UTC().AddDate(0, 0, 1)
		before = &value
	}
	if after != nil && before != nil && !after.Before(*before) {
		return nil, nil, errors.New("invalid source date range")
	}
	return after, before, nil
}

func sweepClassAllowed(classes []SourceClass, lane SourceClass) bool {
	if len(classes) == 0 {
		return true
	}
	return slices.Contains(classes, lane)
}

func contextAllowsItem(item EvidenceItem, classes []SourceClass, after, before *time.Time) bool {
	if !sweepClassAllowed(classes, item.SourceClass) {
		return false
	}
	if after != nil && item.EventTime.Before(*after) {
		return false
	}
	if before != nil && !item.EventTime.Before(*before) {
		return false
	}
	return true
}

func contextQuery(request ContextRequest) string {
	parts := []string{request.Target.Description, request.Target.Slug, request.Target.Key}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func evidenceCoordinateKey(ref EvidenceRef) string {
	return fmt.Sprintf("%s/%d/%d/%d/%s/%s/%d/%d", ref.SourceLane, ref.SourceID,
		ref.MessageID, ref.AttachmentID, ref.OccurrenceKey, ref.ChunkKey, ref.SpanStart, ref.SpanEnd)
}
