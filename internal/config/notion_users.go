package config

import (
	"fmt"
	"os"
	"strings"
)

// ResolveUsersToken reads an optional user-only credential on the daemon host.
// Secret values are never placed in errors or persisted into config.
func (s NotionMeetingsSource) ResolveUsersToken() (string, error) {
	env, file := strings.TrimSpace(s.UsersTokenEnv), strings.TrimSpace(s.UsersTokenFile)
	if env != "" && file != "" {
		return "", fmt.Errorf("notion meeting source %q: configure only one of users_token_env and users_token_file", s.Identifier)
	}
	if env != "" {
		token := strings.TrimSpace(os.Getenv(env))
		if token == "" {
			return "", fmt.Errorf("notion meeting source %q: users_token_env %q is unset or empty", s.Identifier, env)
		}
		return token, nil
	}
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("notion meeting source %q: read users_token_file: %w", s.Identifier, err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("notion meeting source %q: users_token_file is empty", s.Identifier)
		}
		return token, nil
	}
	return "", nil
}
