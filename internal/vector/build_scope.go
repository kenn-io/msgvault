package vector

import (
	"slices"
	"strconv"
	"strings"
)

// BuildScope limits which messages are eligible for an embedding
// generation. A zero-value scope means the full corpus.
type BuildScope struct {
	MessageTypes []string
	// SourceIDs restricts the build to messages belonging to the given
	// sources (accounts). Source IDs are DB-local integers: deterministic
	// within one archive, but deleting and re-adding an account (or
	// restoring a backup) can renumber them, which changes the scope
	// fingerprint and forces a rebuild.
	SourceIDs []int64
}

// NewBuildScope returns a normalized, stable scope. Message types are
// lowercase, trimmed, de-duplicated, and sorted; source IDs are
// de-duplicated, sorted ascending, with non-positive IDs dropped — so
// fingerprints and SQL bindings are deterministic.
func NewBuildScope(messageTypes []string, sourceIDs []int64) BuildScope {
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
	slices.Sort(out)

	idSeen := make(map[int64]struct{}, len(sourceIDs))
	ids := make([]int64, 0, len(sourceIDs))
	for _, id := range sourceIDs {
		if id <= 0 {
			continue
		}
		if _, ok := idSeen[id]; ok {
			continue
		}
		idSeen[id] = struct{}{}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return BuildScope{MessageTypes: out, SourceIDs: ids}
}

func (s BuildScope) IsEmpty() bool {
	return len(s.MessageTypes) == 0 && len(s.SourceIDs) == 0
}

// Fingerprint renders the scope as "mt-<types>" and/or "src-<ids>" segments
// joined by ":". The empty scope fingerprints to "".
func (s BuildScope) Fingerprint() string {
	if s.IsEmpty() {
		return ""
	}
	segs := make([]string, 0, 2)
	if len(s.MessageTypes) > 0 {
		segs = append(segs, "mt-"+strings.Join(s.MessageTypes, ","))
	}
	if len(s.SourceIDs) > 0 {
		var b strings.Builder
		b.WriteString("src-")
		for i, id := range s.SourceIDs {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(strconv.FormatInt(id, 10))
		}
		segs = append(segs, b.String())
	}
	return strings.Join(segs, ":")
}

func (s BuildScope) ContainsMessageType(messageType string) bool {
	messageType = strings.TrimSpace(strings.ToLower(messageType))
	return slices.Contains(s.MessageTypes, messageType)
}

func (s BuildScope) ContainsSource(sourceID int64) bool {
	return slices.Contains(s.SourceIDs, sourceID)
}

// AllowsMessageTypes reports whether the scope's message-type dimension
// permits all the given types. A scope with no message-type restriction
// (including a sources-only scope) allows everything.
func (s BuildScope) AllowsMessageTypes(messageTypes []string) bool {
	if len(s.MessageTypes) == 0 {
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
