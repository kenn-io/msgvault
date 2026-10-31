package skills

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/msgvault/internal/fileutil"
)

const briefingCommand = "msgvault timeline --brief"

// InstallBriefing merges one session-start briefing into a Claude settings.json
// or Codex hooks.json file. Existing settings and hook handlers are retained.
func InstallBriefing(settingsPath string) error {
	return updateBriefing(settingsPath, true)
}

// UninstallBriefing removes exact briefingCommand handlers from SessionStart.
// A missing file or a file with no matching handler is left untouched.
func UninstallBriefing(settingsPath string) error {
	return updateBriefing(settingsPath, false)
}

func updateBriefing(settingsPath string, install bool) error {
	data, err := os.ReadFile(settingsPath)
	if errors.Is(err, os.ErrNotExist) {
		if !install {
			return nil
		}
		data = []byte("{}")
	} else if err != nil {
		return fmt.Errorf("read briefing settings %s: %w", settingsPath, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	// Unknown settings may contain integer IDs beyond float64's exact range.
	decoder.UseNumber()
	var settings map[string]any
	if err := decoder.Decode(&settings); err != nil {
		return fmt.Errorf("decode briefing settings %s: %w", settingsPath, err)
	}
	if settings == nil {
		return fmt.Errorf("briefing settings %s must be a JSON object", settingsPath)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("briefing settings %s must contain exactly one JSON object", settingsPath)
	}

	hooks := map[string]any{}
	if value, exists := settings["hooks"]; exists {
		var ok bool
		hooks, ok = value.(map[string]any)
		if !ok {
			return fmt.Errorf("briefing settings %s: hooks must be an object", settingsPath)
		}
	}
	groups := []any{}
	if value, exists := hooks["SessionStart"]; exists {
		var ok bool
		groups, ok = value.([]any)
		if !ok {
			return fmt.Errorf("briefing settings %s: SessionStart must be an array", settingsPath)
		}
	}
	found := false
	remaining := make([]any, 0, len(groups))
	for _, value := range groups {
		group, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("briefing settings %s: SessionStart groups must be objects", settingsPath)
		}
		handlers, ok := group["hooks"].([]any)
		if !ok {
			return fmt.Errorf("briefing settings %s: a SessionStart group's hooks must be an array", settingsPath)
		}
		kept := make([]any, 0, len(handlers))
		removed := false
		for _, value := range handlers {
			handler, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("briefing settings %s: SessionStart handlers must be objects", settingsPath)
			}
			command, _ := handler["command"].(string)
			match := command == briefingCommand
			found = found || match
			if match && !install {
				removed = true
				continue
			}
			kept = append(kept, handler)
		}
		if removed && len(kept) == 0 {
			// Prune an emptied matcher group, but retain unknown group metadata.
			unknown := false
			for key := range group {
				unknown = unknown || (key != "hooks" && key != "matcher")
			}
			if !unknown {
				continue
			}
		}
		if removed {
			group["hooks"] = kept
		}
		remaining = append(remaining, group)
	}
	if install {
		if found {
			return nil
		}
		groups = append(groups, map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": briefingCommand,
		}}})
		hooks["SessionStart"] = groups
	} else {
		if !found {
			return nil
		}
		if len(remaining) == 0 {
			delete(hooks, "SessionStart")
		} else {
			hooks["SessionStart"] = remaining
		}
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}
	encoded, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode briefing settings %s: %w", settingsPath, err)
	}
	encoded = append(encoded, '\n')
	perm := os.FileMode(0o600)
	if info, err := os.Stat(settingsPath); err == nil {
		perm = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat briefing settings %s: %w", settingsPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		return fmt.Errorf("create briefing settings directory: %w", err)
	}
	if err := fileutil.SecureReplaceFile(settingsPath, encoded, perm); err != nil {
		return fmt.Errorf("write briefing settings %s: %w", settingsPath, err)
	}
	return nil
}
