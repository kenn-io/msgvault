package personobs

import "strings"

// Render substitutes names without interpreting placeholder tokens in the names.
func Render(content, personName, ownerName string) string {
	return strings.NewReplacer("[PERSON]", personName, "[OWNER]", ownerName).Replace(CanonicalizePlaceholders(content))
}
