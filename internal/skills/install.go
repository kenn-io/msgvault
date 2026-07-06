package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const skillFileName = "SKILL.md"

// AgentDir is one agent's skills root directory.
type AgentDir struct {
	Agent string // "claude" or "codex"
	Dir   string // skills root, e.g. ~/.claude/skills
}

// knownAgents maps agent names to their config directory under $HOME.
var knownAgents = []struct{ name, configDir string }{
	{"claude", ".claude"},
	{"codex", ".codex"},
}

// DetectAgents returns the skills roots of agents whose config
// directory exists under home. only restricts detection to the named
// agents; empty means all known agents. Unknown names are an error.
func DetectAgents(home string, only []string) ([]AgentDir, error) {
	filter := make(map[string]bool, len(only))
	for _, name := range only {
		valid := false
		for _, a := range knownAgents {
			if a.name == name {
				valid = true
				break
			}
		}
		if !valid {
			return nil, fmt.Errorf(
				"unknown agent %q (valid agents: claude, codex)", name)
		}
		filter[name] = true
	}
	var out []AgentDir
	for _, a := range knownAgents {
		if len(filter) > 0 && !filter[a.name] {
			continue
		}
		configDir := filepath.Join(home, a.configDir)
		info, err := os.Stat(configDir)
		if err != nil || !info.IsDir() {
			continue
		}
		out = append(out, AgentDir{
			Agent: a.name,
			Dir:   filepath.Join(configDir, "skills"),
		})
	}
	return out, nil
}

// InstallStatus describes what Install did with one skill file.
type InstallStatus string

// Install outcomes.
const (
	StatusInstalled InstallStatus = "installed"
	StatusUpdated   InstallStatus = "updated"
	StatusSkipped   InstallStatus = "skipped"
)

// InstallResult reports the outcome for one skill.
type InstallResult struct {
	Skill  string
	Path   string
	Status InstallStatus
}

// Install writes each skill to <root>/<name>/SKILL.md. An existing
// file is overwritten only when it contains Marker (i.e. we wrote it)
// or when force is set; otherwise it is reported as skipped.
func Install(root string, list []Skill, force bool) ([]InstallResult, error) {
	results := make([]InstallResult, 0, len(list))
	for _, sk := range list {
		dir := filepath.Join(root, sk.Name)
		path := filepath.Join(dir, skillFileName)
		status := StatusInstalled
		existing, err := os.ReadFile(path)
		switch {
		case err == nil && !force && !strings.Contains(string(existing), Marker):
			results = append(results,
				InstallResult{Skill: sk.Name, Path: path, Status: StatusSkipped})
			continue
		case err == nil:
			status = StatusUpdated
		case !errors.Is(err, os.ErrNotExist):
			return results, fmt.Errorf("read existing skill %s: %w", path, err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return results, fmt.Errorf("create skill directory %s: %w", dir, err)
		}
		if err := os.WriteFile(path, []byte(sk.Content), 0o600); err != nil {
			return results, fmt.Errorf("write skill %s: %w", path, err)
		}
		results = append(results,
			InstallResult{Skill: sk.Name, Path: path, Status: status})
	}
	return results, nil
}

// Uninstall removes every msgvault-* skill directory under root whose
// SKILL.md contains Marker. Hand-authored files are left in place.
func Uninstall(root string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(root, "msgvault-*", skillFileName))
	if err != nil {
		return nil, fmt.Errorf("scan skills root %s: %w", root, err)
	}
	var removed []string
	for _, path := range matches {
		content, err := os.ReadFile(path)
		if err != nil {
			return removed, fmt.Errorf("read skill %s: %w", path, err)
		}
		if !strings.Contains(string(content), Marker) {
			continue
		}
		dir := filepath.Dir(path)
		if err := os.RemoveAll(dir); err != nil {
			return removed, fmt.Errorf("remove skill directory %s: %w", dir, err)
		}
		removed = append(removed, dir)
	}
	return removed, nil
}
