package api

// IsCLIRunIntro identifies the whole group, including invalid actions, so no
// introduction arguments can fall through to a subprocess or generic logs.
func IsCLIRunIntro(args []string) bool { return len(args) > 0 && args[0] == "intro" }
func introActionAllowed(args []string) bool {
	if len(args) < 2 {
		return false
	}
	switch args[1] {
	case "prepare", "list", "show", "edit", "cancel":
		return true
	}
	return false
}
