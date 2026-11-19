// Package contactcadence defines the shared contact-list defaults.
package contactcadence

import "strings"

// Preset is a deliberate contact list stored as a day interval.
type Preset struct {
	List string `json:"list"`
	Days int64  `json:"days"`
}

// Presets returns an independent copy in list order.
func Presets() []Preset { return []Preset{{"A", 21}, {"B", 60}, {"C", 180}, {"D", 365}} }

// DaysForList resolves a list letter; custom intervals have no letter.
func DaysForList(list string) (int64, bool) {
	for _, preset := range Presets() {
		if preset.List == strings.ToUpper(strings.TrimSpace(list)) {
			return preset.Days, true
		}
	}
	return 0, false
}

// ListForDays returns the exact matching preset, or empty for a custom interval.
func ListForDays(days int64) string {
	for _, preset := range Presets() {
		if preset.Days == days {
			return preset.List
		}
	}
	return ""
}
