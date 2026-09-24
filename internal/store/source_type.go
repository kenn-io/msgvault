package store

// EffectiveSourceType returns the provider type used by compatibility-aware
// source consumers. Empty source types are the historical Gmail spelling.
func EffectiveSourceType(sourceType string) string {
	if sourceType == "" {
		return "gmail"
	}
	return sourceType
}
