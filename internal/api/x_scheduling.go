package api

import "context"

// sourceScheduling keeps typed source status and triggers aligned with the
// daemon's exact X-owner precedence. Unqualified legacy account triggers retain
// their existing identifier dispatch; an unrelated typed source cannot borrow
// an X owner's job merely because its identifier is the same numeric string.
func (s *Server) sourceScheduling(ctx context.Context, sourceType, identifier string) (sourceScheduleClassification, error) {
	classification := classifySourceScheduling(sourceType, identifier)
	if classification.kind != sourceScheduleAccount || sourceType == "" {
		return classification, nil
	}
	lister, ok := s.store.(SourceStatusStore)
	if !ok {
		if sourceType == "x" {
			return sourceScheduleClassification{kind: sourceScheduleNonSchedulable}, nil
		}
		return classification, nil
	}
	sources, err := lister.ListSourcesContext(ctx, "x")
	if err != nil {
		return sourceScheduleClassification{}, err
	}
	for _, source := range sources {
		if source.SourceType == "x" && source.Identifier == identifier {
			if sourceType != "x" || source.MergedIntoSourceID != 0 || source.HistoryOnly {
				return sourceScheduleClassification{kind: sourceScheduleNonSchedulable}, nil
			}
			return classification, nil
		}
	}
	if sourceType == "x" {
		return sourceScheduleClassification{kind: sourceScheduleNonSchedulable}, nil
	}
	return classification, nil
}
