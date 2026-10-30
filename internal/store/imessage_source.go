package store

// SelectImessageSource selects the existing source used by the singleton
// Messages import: the newest non-local source, or the local source.
// The caller creates a local source when no existing source is selected.
func SelectImessageSource(sources []*Source) *Source {
	var best *Source
	for _, source := range sources {
		if source.Identifier != "local" && (best == nil || source.ID > best.ID) {
			best = source
		}
	}
	if best != nil {
		return best
	}
	if len(sources) > 0 {
		return sources[0]
	}
	return nil
}
