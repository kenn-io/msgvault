package store

// SelectImessageSource selects the existing source used by the singleton
// Messages import: the newest non-local source, or the local source. Sources
// merged into another are retired and never selected.
// The caller creates a local source when no existing source is selected.
func SelectImessageSource(sources []*Source) *Source {
	var best, local *Source
	for _, source := range sources {
		switch {
		case source.MergedIntoSourceID != 0:
		case source.Identifier != "local":
			if best == nil || source.ID > best.ID {
				best = source
			}
		case local == nil:
			local = source
		}
	}
	if best != nil {
		return best
	}
	return local
}
