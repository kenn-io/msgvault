package main

import (
	"errors"
	"regexp"
	"strings"

	"go.kenn.io/msgvault/internal/mcp"
)

var (
	grantExample   = regexp.MustCompile(`mva1_[A-Za-z0-9_-]+`)
	bearerExample  = regexp.MustCompile(`Bearer\s+(\S+)`)
	addressExample = regexp.MustCompile(`[A-Za-z0-9._%+-]+@([A-Za-z0-9.-]+\.[A-Za-z]+)`)
	guideReference = regexp.MustCompile("`([a-z][a-z0-9]*(?:_[a-z0-9]+)+)`")
)

// validateGuide runs before the published guide is written.
// Refusal messages deliberately omit credential-like values from the input.
func validateGuide(text string, tools []mcp.ToolSummary) error {
	if grantExample.MatchString(text) {
		return errors.New("guide contains a grant credential; use mva1_... as a placeholder")
	}
	for _, match := range bearerExample.FindAllStringSubmatch(text, -1) {
		if strings.Trim(match[1], "\"'`.,)") != "<key>" {
			return errors.New("guide contains a bearer credential; use <key> as a placeholder")
		}
	}
	for _, match := range addressExample.FindAllStringSubmatch(text, -1) {
		domain := strings.ToLower(match[1])
		if domain != "example.com" && domain != "example.org" && domain != "example.net" && !strings.HasSuffix(domain, ".example.com") && !strings.HasSuffix(domain, ".example.org") && !strings.HasSuffix(domain, ".example.net") && !strings.HasSuffix(domain, ".test") {
			return errors.New("guide contains a non-example email address")
		}
	}
	known := make(map[string]bool, len(tools))
	for _, tool := range tools {
		known[tool.Name] = true
	}
	// Input and response fields documented in the narrative are not tool names.
	for _, field := range []string{"person_id", "vcard_uid", "participant_id", "person_uid", "source_message_id", "thread_id", "body_returned", "body_length", "has_more", "group_by", "job_id", "message_id", "draft_id", "allow_send", "reply_to"} {
		known[field] = true
	}
	prose := text
	if prefix, rest, ok := strings.Cut(text, beginTools); ok {
		if _, suffix, ok := strings.Cut(rest, endTools); ok {
			prose = prefix + suffix
		}
	}
	for _, match := range guideReference.FindAllStringSubmatch(prose, -1) {
		if !known[match[1]] {
			return errors.New("guide contains an unknown tool or guide field")
		}
	}
	return nil
}
