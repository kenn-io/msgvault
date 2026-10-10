package mcpevents

import (
	"encoding/json/v2"
	"slices"

	"go.kenn.io/msgvault/internal/store"
)

const messageFamily = "msgvault.message_archived"
const calendarFamily = "msgvault.calendar_event_changed"
const draftFamily = "msgvault.draft_changed"

type arguments struct {
	bytes     []byte
	scopeKind string
	scopeID   int64
	kinds     []string
}

func canonicalArguments(name string, input map[string]any) (arguments, error) {
	var a arguments
	key := "conversation_id"
	a.scopeKind = "conversation"
	allowed := []string{key}
	switch name {
	case messageFamily:
		allowed = append(allowed, "include_from_me", "include_reactions")
	case calendarFamily:
		key = "calendar_source_id"
		a.scopeKind = "source"
		allowed = []string{key}
	case draftFamily:
		allowed = append(allowed, "kinds")
	default:
		return a, invalid("unknown_event")
	}
	for field := range input {
		if !slices.Contains(allowed, field) {
			return a, invalid("unknown_argument")
		}
	}
	id, ok := input[key].(string)
	if !ok {
		return a, invalid("invalid_scope")
	}
	var err error
	a.scopeID, err = decimal(id)
	if err != nil || a.scopeID == 0 {
		return a, invalid("invalid_scope")
	}
	canonical := map[string]any{key: id}
	if name == messageFamily {
		for field, fallback := range map[string]bool{"include_from_me": false, "include_reactions": true} {
			value := fallback
			if raw, exists := input[field]; exists {
				var ok bool
				value, ok = raw.(bool)
				if !ok {
					return a, invalid("invalid_boolean")
				}
			}
			canonical[field] = value
		}
	}
	if name == draftFamily {
		a.kinds = []string{"*"}
		if raw, exists := input["kinds"]; exists {
			var kinds []string
			switch v := raw.(type) {
			case []string:
				kinds = append(kinds, v...)
			case []any:
				for _, item := range v {
					kind, ok := item.(string)
					if !ok {
						return a, invalid("invalid_kinds")
					}
					kinds = append(kinds, kind)
				}
			default:
				return a, invalid("invalid_kinds")
			}
			if len(kinds) == 0 {
				return a, invalid("invalid_kinds")
			}
			for _, kind := range kinds {
				if !slices.Contains([]string{"created", "updated", "deleted"}, kind) {
					return a, invalid("invalid_kinds")
				}
			}
			slices.Sort(kinds)
			a.kinds = slices.Compact(kinds)
		}
		canonical["kinds"] = a.kinds
	}
	a.bytes, err = json.Marshal(canonical, json.Deterministic(true))
	return a, err
}
func capabilities(enabled bool, sources []string) []store.MCPEventCapability {
	result := make([]store.MCPEventCapability, 0)
	if !enabled {
		return result
	}
	for _, source := range sources {
		switch source {
		case "gmail", "imap":
			result = append(result, store.MCPEventCapability{Family: messageFamily, SourceType: source, Kinds: []string{"message"}}, store.MCPEventCapability{Family: draftFamily, SourceType: source, Kinds: []string{"created", "updated", "deleted"}})
		case "beeper", "slack", "slackdump", "teams", "discord":
			result = append(result, store.MCPEventCapability{Family: draftFamily, SourceType: source, Kinds: []string{"created", "updated", "deleted"}})
		case "gcal":
			result = append(result, store.MCPEventCapability{Family: calendarFamily, SourceType: source, Kinds: []string{"created", "updated", "cancelled"}})
		}
	}
	return result
}

func (s *Service) Capabilities() []store.MCPEventCapability {
	result := make([]store.MCPEventCapability, len(s.caps))
	for i, c := range s.caps {
		result[i] = c
		result[i].Kinds = append([]string(nil), c.Kinds...)
	}
	return result
}

const schemaTypeProperty = "type"

func schemaString() map[string]any { return map[string]any{schemaTypeProperty: "string"} }
func schemaID() map[string]any {
	return map[string]any{schemaTypeProperty: "string", "pattern": "^[1-9][0-9]{0,18}$"}
}
func schemaNullable(kind string) map[string]any {
	return map[string]any{schemaTypeProperty: []string{kind, "null"}}
}
func closedObject(properties map[string]any, required []string) map[string]any {
	return map[string]any{schemaTypeProperty: "object", "properties": properties, "required": required, "additionalProperties": false}
}
func (s *Service) Catalog() ListResult {
	result := ListResult{Events: make([]Definition, 0)}
	for _, family := range []string{messageFamily, calendarFamily, draftFamily} {
		var kinds, draftKinds []string
		for _, cap := range s.caps {
			if cap.Family == family {
				kinds = append(kinds, cap.Kinds...)
				if family == draftFamily {
					switch cap.SourceType {
					case "gmail", "imap", "beeper":
						draftKinds = append(draftKinds, cap.SourceType)
					case "slack", "slackdump", "teams", "discord":
						draftKinds = append(draftKinds, "chat")
					}
				}
			}
		}
		if len(kinds) == 0 {
			continue
		}
		slices.Sort(kinds)
		kinds = slices.Compact(kinds)
		slices.Sort(draftKinds)
		draftKinds = slices.Compact(draftKinds)
		props := map[string]any{"conversation_id": schemaID()}
		required := []string{"conversation_id"}
		payload := map[string]any{"kind": map[string]any{schemaTypeProperty: "string", "enum": kinds}, "conversation_id": schemaID(), "source_id": schemaID()}
		payloadRequired := []string{"kind", "conversation_id", "source_id"}
		description := "A newly archived message in one conversation. Read content with get_message and list_thread; read attachments with get_attachment. get_mcp_event recovers a lost payload from eventId."
		switch family {
		case messageFamily:
			props["include_from_me"] = map[string]any{schemaTypeProperty: "boolean", "default": false}
			props["include_reactions"] = map[string]any{schemaTypeProperty: "boolean", "default": true}
			payload["from_me"] = map[string]any{schemaTypeProperty: "boolean"}
			payload["message_id"] = schemaID()
			payload["sent_at"] = schemaNullable("string")
			payload["archived_at"] = schemaString()
			payloadRequired = append(payloadRequired, "from_me", "message_id", "sent_at", "archived_at")
		case calendarFamily:
			props = map[string]any{"calendar_source_id": schemaID()}
			required = []string{"calendar_source_id"}
			for _, field := range []string{"ical_uid", "starts_at"} {
				payload[field] = schemaNullable("string")
			}
			payload["sequence"] = schemaNullable("integer")
			payload["all_day"] = schemaNullable("boolean")
			payload["message_id"] = schemaID()
			payload["changed_at"] = schemaString()
			payload["from_me"] = map[string]any{schemaTypeProperty: "boolean"}
			payloadRequired = append(payloadRequired, "message_id", "ical_uid", "sequence", "starts_at", "all_day", "changed_at", "from_me")
			description = "A calendar object created, updated, or cancelled in one synced calendar. Resolve its calendar_source_id with list_calendar_sources; read its current calendar projection with get_message. get_mcp_event recovers a lost payload from eventId."
		case draftFamily:
			props["kinds"] = map[string]any{schemaTypeProperty: "array", "minItems": 1, "items": map[string]any{schemaTypeProperty: "string", "enum": kinds}}
			payload["draft_id"] = schemaString()
			payload["revision"] = map[string]any{schemaTypeProperty: "integer"}
			payload["draft_kind"] = map[string]any{schemaTypeProperty: "string", "enum": draftKinds}
			payload["message_id"] = schemaID()
			payload["changed_at"] = schemaString()
			payload["created_by"] = schemaString()
			payloadRequired = append(payloadRequired, "draft_id", "revision", "draft_kind", "changed_at", "created_by")
			description = "A managed draft confirmed created, updated, or deleted in one conversation. Read its current content with draft_get."
			if slices.Contains(draftKinds, "gmail") || slices.Contains(draftKinds, "imap") {
				description += " Read its archived email draft row with get_message."
			}
			description += " get_mcp_event recovers a lost payload from eventId even after draft deletion."
		}
		payloadSchema := closedObject(payload, payloadRequired)
		if family == draftFamily {
			payloadSchema["if"] = map[string]any{"properties": map[string]any{"draft_kind": map[string]any{"enum": []string{"gmail", "imap"}}}}
			payloadSchema["then"] = map[string]any{"required": []string{"message_id"}}
			payloadSchema["else"] = map[string]any{"not": map[string]any{"required": []string{"message_id"}}}
		}
		result.Events = append(result.Events, Definition{Name: family, Description: description, Delivery: []string{"webhook"}, InputSchema: closedObject(props, required), PayloadSchema: payloadSchema})
	}
	return result
}
