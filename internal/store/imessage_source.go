package store

// SelectImessageSource prefers the newest active non-local Messages source,
// then the local source. Merged sources have retired from ingestion.
func SelectImessageSource(sources []*Source) *Source {
	var best, local *Source
	for _, src := range sources {
		if src.MergedIntoSourceID != 0 {
			continue
		}
		if src.Identifier == "local" {
			local = src
		} else if best == nil || src.ID > best.ID {
			best = src
		}
	}
	if best != nil {
		return best
	}
	return local
}
