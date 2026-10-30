package api

import (
	"errors"
	"strconv"
	"strings"
)

const CLIRunSendReplyCommand = "send-reply"

// SendReplyIntent describes either a direct reply to an archived message or a
// reviewed local chat draft. The archived message or draft determines the
// source; callers cannot select a different source.
type SendReplyIntent struct {
	MessageID    int64
	DraftID      string
	Revision     int64
	Body         string
	AdoptMediaID string
	Quota        bool
	JSON         bool
}

func IsCLIRunSendReply(args []string) bool {
	return len(args) > 0 && args[0] == CLIRunSendReplyCommand
}

// ParseCLIRunSendReply accepts a positional archived-message ID plus --body,
// or --draft plus --revision. Quota inspection is kept as a source-scoped
// extension and also derives its source from a positional archived message.
func ParseCLIRunSendReply(req CLIRunRequest) (SendReplyIntent, error) {
	var result SendReplyIntent
	invalid := errors.New("invalid send-reply arguments")
	if !IsCLIRunSendReply(req.Args) || req.Cwd != "" || len(req.Env) != 0 || req.GrantDecided {
		return result, invalid
	}
	seen := map[string]bool{}
	args := req.Args[1:]
	positionalOnly := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if positionalOnly || !strings.HasPrefix(arg, "--") || arg == "--" {
			if arg == "--" && !positionalOnly {
				positionalOnly = true
				continue
			}
			if result.MessageID != 0 {
				return result, invalid
			}
			id, err := strconv.ParseInt(arg, 10, 64)
			if err != nil || id <= 0 {
				return result, invalid
			}
			result.MessageID = id
			continue
		}
		key, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if seen[key] {
			return result, invalid
		}
		seen[key] = true
		switch key {
		case "quota", "json", "verbose", "log-sql":
			enabled := true
			if hasValue {
				parsed, err := strconv.ParseBool(value)
				if err != nil {
					return result, invalid
				}
				enabled = parsed
			}
			if key == "quota" {
				result.Quota = enabled
			}
			if key == "json" {
				result.JSON = enabled
			}
		case "draft", "revision", "body", "adopt-media-id", "log-level", "log-sql-slow-ms":
			if !hasValue {
				i++
				if i >= len(args) {
					return result, invalid
				}
				value = args[i]
			}
			switch key {
			case "draft":
				if !strings.HasPrefix(value, "chat-draft-") || len(value) == len("chat-draft-") {
					return result, invalid
				}
				result.DraftID = value
			case "revision":
				n, err := strconv.ParseInt(value, 10, 64)
				if err != nil || n <= 0 {
					return result, invalid
				}
				result.Revision = n
			case "body":
				result.Body = value
			case "adopt-media-id":
				if len(value) == 0 || len(value) > 128 || strings.Trim(value, "0") == "" {
					return result, invalid
				}
				for _, r := range value {
					if r < '0' || r > '9' {
						return result, invalid
					}
				}
				result.AdoptMediaID = value
			}
		default:
			return result, invalid
		}
	}

	if seen["quota"] && !result.Quota {
		return result, invalid
	}
	if result.DraftID != "" {
		if result.MessageID != 0 || result.Revision <= 0 || !seen["revision"] || seen["body"] || result.Quota {
			return result, invalid
		}
		return result, nil
	}
	if result.MessageID <= 0 || seen["revision"] || seen["draft"] || seen["adopt-media-id"] {
		return result, invalid
	}
	if result.Quota {
		if seen["body"] {
			return result, invalid
		}
		return result, nil
	}
	if !seen["body"] || result.Body == "" {
		return result, invalid
	}
	return result, nil
}
