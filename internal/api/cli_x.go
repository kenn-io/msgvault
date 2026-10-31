package api

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/config"
	xapi "go.kenn.io/msgvault/internal/x"
)

type XRegisterRequest struct {
	UserID string
	JSON   bool
}

// IsCLIRunXRegister identifies every add-x invocation so unsafe forms cannot
// fall through to the daemon subprocess runner.
func IsCLIRunXRegister(args []string) bool { return len(args) > 0 && args[0] == "add-x" }

// ParseCLIRunXRegister admits only host-local credential registration. OAuth
// stays on the browser machine; no credential, path, environment or cwd crosses
// the CLI-run boundary.
func ParseCLIRunXRegister(req CLIRunRequest) (XRegisterRequest, error) {
	invalid := errors.New("X registration accepts only --register, --user-id and optional --json with no environment or working directory")
	if !IsCLIRunXRegister(req.Args) || len(req.Env) > 0 || req.Cwd != "" || req.Grant != nil || req.GrantDecided {
		return XRegisterRequest{}, invalid
	}
	var out XRegisterRequest
	seen := make(map[string]bool)
	registered := false
	args := req.Args[1:]
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		if !strings.HasPrefix(arg, "--") {
			return XRegisterRequest{}, invalid
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if seen[name] {
			return XRegisterRequest{}, invalid
		}
		seen[name] = true
		switch name {
		case "register":
			if hasValue && value != "true" {
				return XRegisterRequest{}, invalid
			}
			registered = true
		case "user-id":
			if !hasValue {
				if len(args) == 0 {
					return XRegisterRequest{}, invalid
				}
				value, args = args[0], args[1:]
			}
			out.UserID = value
		case "json":
			if hasValue && value != "true" && value != "false" {
				return XRegisterRequest{}, invalid
			}
			out.JSON = !hasValue || value == "true"
		default:
			return XRegisterRequest{}, invalid
		}
	}
	if !registered || xapi.ValidateID(out.UserID) != nil {
		return XRegisterRequest{}, invalid
	}
	return out, nil
}

// XSyncRequest describes collection, archive work or an exclusive local action.
type XSyncRequest struct {
	ExportRemovals                                       bool
	Archive, NewJob                                      bool
	MaxSpendMicros                                       *int64
	JobID                                                string
	RemovePost                                           string
	SourceID                                             int64
	After                                                time.Time
	JSON, BuildCache, NoBuildCache, AcknowledgeReadModel bool
}

func IsCLIRunXSync(args []string) bool { return len(args) > 0 && args[0] == "sync-x" }

// ParseCLIRunXSync rejects every unsupported shape before daemon dispatch.
func ParseCLIRunXSync(req CLIRunRequest) (XSyncRequest, error) {
	invalid := errors.New("X sync requires --source-id except exclusive archive-wide --export-removals; archive requires --after and accepts optional --max-spend, --job-id or --new-job; local maintenance actions are exclusive; environment, working directory and delegation are forbidden")
	if !IsCLIRunXSync(req.Args) || len(req.Env) > 0 || req.Cwd != "" || req.Grant != nil || req.GrantDecided {
		return XSyncRequest{}, invalid
	}
	var out XSyncRequest
	seen := make(map[string]bool)
	args := req.Args[1:]
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		if !strings.HasPrefix(arg, "--") {
			return XSyncRequest{}, invalid
		}
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if seen[name] {
			return XSyncRequest{}, invalid
		}
		seen[name] = true
		switch name {
		case "source-id", "after", "max-spend", "job-id", "remove-post":
			if !hasValue {
				if len(args) == 0 {
					return XSyncRequest{}, invalid
				}
				value, args = args[0], args[1:]
			}
			if name == "source-id" {
				id, err := strconv.ParseInt(value, 10, 64)
				if err != nil || id <= 0 || strconv.FormatInt(id, 10) != value {
					return XSyncRequest{}, invalid
				}
				out.SourceID = id
			} else if name == "remove-post" {
				if xapi.ValidateID(value) != nil {
					return XSyncRequest{}, invalid
				}
				out.RemovePost = value
			} else if name == "max-spend" {
				micros, err := config.ParseUSDMicros(value)
				if err != nil {
					return XSyncRequest{}, fmt.Errorf("--max-spend: %w", err)
				}
				out.MaxSpendMicros = &micros
			} else if name == "job-id" {
				if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
					return XSyncRequest{}, invalid
				}
				out.JobID = value
			} else {
				parsed, err := time.Parse(time.RFC3339, value)
				if err != nil {
					parsed, err = time.Parse("2006-01-02", value)
				}
				if err != nil || parsed.IsZero() {
					return XSyncRequest{}, invalid
				}
				out.After = parsed.UTC()
			}
		case "json", "build-cache", "no-build-cache", "acknowledge-read-model", "archive", "new-job", "export-removals":
			if hasValue && value != "true" && value != "false" {
				return XSyncRequest{}, invalid
			}
			enabled := !hasValue || value == "true"
			switch name {
			case "export-removals":
				if !enabled {
					return XSyncRequest{}, invalid
				}
				out.ExportRemovals = true
			case "archive":
				out.Archive = enabled
			case "new-job":
				out.NewJob = enabled
			case "json":
				out.JSON = enabled
			case "build-cache":
				out.BuildCache = enabled
			case "no-build-cache":
				out.NoBuildCache = enabled
			case "acknowledge-read-model":
				if !enabled {
					return XSyncRequest{}, invalid
				}
				out.AcknowledgeReadModel = true
			}
		default:
			return XSyncRequest{}, invalid
		}
	}
	if out.ExportRemovals {
		for _, name := range []string{"source-id", "after", "archive", "max-spend", "job-id", "new-job", "build-cache", "no-build-cache", "acknowledge-read-model", "remove-post"} {
			if seen[name] {
				return XSyncRequest{}, invalid
			}
		}
		return out, nil
	}
	if out.RemovePost != "" {
		for _, name := range []string{"after", "archive", "max-spend", "job-id", "new-job", "build-cache", "no-build-cache", "acknowledge-read-model"} {
			if seen[name] {
				return XSyncRequest{}, invalid
			}
		}
	}
	if (out.Archive && !seen["after"]) || (!out.Archive && (seen["max-spend"] || seen["job-id"] || seen["new-job"])) || (seen["job-id"] && seen["new-job"]) {
		return XSyncRequest{}, invalid
	}
	if out.SourceID == 0 || (seen["build-cache"] && seen["no-build-cache"]) || (out.AcknowledgeReadModel && (seen["after"] || seen["build-cache"] || seen["no-build-cache"] || seen["archive"] || seen["max-spend"] || seen["job-id"] || seen["new-job"])) {
		return XSyncRequest{}, invalid
	}
	return out, nil
}
