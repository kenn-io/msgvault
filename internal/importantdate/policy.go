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

type SubjectPreference string

const (
	SubjectInherit SubjectPreference = "inherit"
	SubjectInclude SubjectPreference = "include"
	SubjectExclude SubjectPreference = "exclude"
)

type Eligibility struct {
	Enabled bool
	Tier    Tier
}

// ResolveEligibility gives explicit exclusion priority over inherited routing.
// Unknown preferences fail closed rather than inheriting implicitly.
func ResolveEligibility(preference SubjectPreference, direct, parent Eligibility) Eligibility {
	var selected Eligibility
	switch preference {
	case SubjectInclude:
		selected = direct
	case SubjectInherit:
		selected = parent
	default:
		return Eligibility{}
	}
	if !selected.Enabled || (selected.Tier != TierA && selected.Tier != TierB && selected.Tier != TierC && selected.Tier != TierD) {
		return Eligibility{}
	}
	return selected
}
