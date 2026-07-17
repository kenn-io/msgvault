package vector

import (
	"slices"
	"sort"
	"strings"
)

// BuildScope limits which messages are eligible for an embedding
// generation. A zero-value scope means the full corpus.
type BuildScope struct {
	MessageTypes []string
}

// NewBuildScope returns a normalized, stable scope. Message types are
// lowercase, trimmed, de-duplicated, and sorted so fingerprints and SQL
// bindings are deterministic.
func NewBuildScope(messageTypes []string) BuildScope {
	seen := make(map[string]struct{}, len(messageTypes))
	out := make([]string, 0, len(messageTypes))
	for _, typ := range messageTypes {
		typ = strings.TrimSpace(strings.ToLower(typ))
		if typ == "" {
			continue
		}
		if _, ok := seen[typ]; ok {
			continue
		}
		seen[typ] = struct{}{}
		out = append(out, typ)
	}
	sort.Strings(out)
	return BuildScope{MessageTypes: out}
}

func (s BuildScope) IsEmpty() bool {
	return len(s.MessageTypes) == 0
}

func (s BuildScope) Fingerprint() string {
	if s.IsEmpty() {
		return ""
	}
	return "mt-" + strings.Join(s.MessageTypes, ",")
}

func (s BuildScope) ContainsMessageType(messageType string) bool {
	messageType = strings.TrimSpace(strings.ToLower(messageType))
	return slices.Contains(s.MessageTypes, messageType)
}

func (s BuildScope) AllowsMessageTypes(messageTypes []string) bool {
	if s.IsEmpty() {
		return true
	}
	if len(messageTypes) == 0 {
		return false
	}
	for _, typ := range messageTypes {
		if !s.ContainsMessageType(typ) {
			return false
		}
	}
	return true
}
