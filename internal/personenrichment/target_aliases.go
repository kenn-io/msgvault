package personenrichment

// personTargetField classifies field aliases. Providers decide which fields,
// descriptor types, and cardinalities they support and whether to use key fallback.
func personTargetField(alias string) string {
	switch alias {
	case "name", "full_name":
		return "name"
	case "first_name", "last_name", "location":
		return alias
	case "education", "education_history":
		return "education"
	default:
		return ""
	}
}
