package importantdate

// Tier names an operator-configured contact importance category.
type Tier string

const (
	TierA Tier = "A"
	TierB Tier = "B"
	TierC Tier = "C"
	TierD Tier = "D"
)

// TierForCadence uses only an explicit mapping. A cadence by itself is not a
// declaration of importance; custom and unassigned values remain unmapped.
func TierForCadence(days int, mapping map[int]Tier) (Tier, bool) {
	if days <= 0 {
		return "", false
	}
	tier, ok := mapping[days]
	if !ok || (tier != TierA && tier != TierB && tier != TierC && tier != TierD) {
		return "", false
	}
	return tier, true
}
