package mcpevents

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/sourcecatalog"
	"go.kenn.io/msgvault/internal/store"
)

const messageFamily = "msgvault.message_archived"
const sourceMessageFamily = "msgvault.source_message_archived"
const inboxMessageFamily = "msgvault.inbox_message_archived"
const calendarFamily = "msgvault.calendar_event_changed"
const draftFamily = "msgvault.draft_changed"

// ReadToolsForCapability returns the MCP readers associated with one event
// family and source type. Health and resource discovery use the same mapping
// so clients see consistent read hints for a subscription scope.
func ReadToolsForCapability(family, sourceType string) []string {
	switch family {
	case messageFamily:
		tools := []string{"get_message", "list_thread"}
		if sourceType != sourcecatalog.Matrix {
			tools = append(tools, "get_attachment")
		}
		return tools
	case sourceMessageFamily, inboxMessageFamily:
		return []string{"get_message", "list_thread"}
	case draftFamily:
		tools := []string{"draft_get"}
		if sourceType == sourcecatalog.Gmail || sourceType == sourcecatalog.IMAP {
			tools = append(tools, "get_message")
		}
		return tools
	default:
		return []string{"get_message"}
	}
}

type arguments struct {
	bytes     []byte
	scopeKind string
	scopeID   int64
	kinds     []string
	reference string
}

func canonicalArguments(name string, input map[string]any) (arguments, error) {
	var a arguments
	key := "conversation_id"
	a.scopeKind = "conversation"
	allowed := []string{key}
	switch name {
	case messageFamily:
		allowed = append(allowed, "include_from_me")
	case sourceMessageFamily:
		key = "source_id"
		a.scopeKind = "source"
		allowed = []string{key, "include_from_me", "include_reactions"}
	case inboxMessageFamily:
		key = "inbox_id"
		a.scopeKind = "inbox"
		allowed = []string{key, "include_from_me", "include_reactions"}
	case calendarFamily:
		key = "calendar_source_id"
		a.scopeKind = "source"
		allowed = []string{key}
	case draftFamily:
		allowed = append(allowed, "kinds")
	default:
		return a, invalid("unknown_event")
	}
	refKey := "conversation_ref"
	if a.scopeKind == "source" {
		refKey = "source_ref"
	}
	if a.scopeKind == "inbox" {
		refKey = "inbox_ref"
	}
	allowed = append(allowed, refKey)
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
	if raw, exists := input[refKey]; exists {
		reference, ok := raw.(string)
		if !ok || !store.CanonicalMCPResourceReference(reference) {
			return a, invalid("invalid_resource_reference")
		}
		a.reference = reference
		canonical[refKey] = reference
	}
	if (name == sourceMessageFamily || name == inboxMessageFamily) && a.reference == "" {
		return a, invalid("invalid_resource_reference")
	}
	if name == messageFamily || name == sourceMessageFamily || name == inboxMessageFamily {
		defaults := map[string]bool{"include_from_me": false}
		if name == sourceMessageFamily || name == inboxMessageFamily {
			defaults["include_reactions"] = true
		}
		for field, fallback := range defaults {
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
	selected := sources
	if len(sources) == 1 && sources[0] == "*" {
		descriptors := sourcecatalog.List()
		selected = make([]string, 0, len(descriptors))
		for _, descriptor := range descriptors {
			selected = append(selected, descriptor.Type)
		}
	}
	for _, source := range selected {
		descriptor, known := sourcecatalog.Lookup(source)
		if !known {
			continue
		}
		if descriptor.LiveMessages {
			result = append(result, store.MCPEventCapability{Family: messageFamily, SourceType: source, Kinds: []string{"message"}})
			kinds := []string{"message"}
			if descriptor.Reactions {
				kinds = append(kinds, "reaction")
			}
			result = append(result, store.MCPEventCapability{Family: sourceMessageFamily, SourceType: source, Kinds: kinds})
			if source == sourcecatalog.Gmail || source == sourcecatalog.MicrosoftMail || source == sourcecatalog.IMAP || source == sourcecatalog.Slack || source == sourcecatalog.Teams || source == sourcecatalog.Discord || source == sourcecatalog.Matrix || source == sourcecatalog.Beeper {
				result = append(result, store.MCPEventCapability{Family: inboxMessageFamily, SourceType: source, Kinds: []string{"message"}})
			}
		}
		if descriptor.Drafts {
			result = append(result, store.MCPEventCapability{Family: draftFamily, SourceType: source, Kinds: []string{"created", "updated", "deleted"}})
		}
		if descriptor.Calendars {
			result = append(result, store.MCPEventCapability{Family: calendarFamily, SourceType: source, Kinds: []string{"created", "updated", "cancelled"}})
		}
	}

	return result
}

// validateSources rejects configured source types that Events cannot capture.
func validateSources(sources []string) error {
	if len(sources) == 1 && sources[0] == "*" {
		return nil
	}
	supported := make([]string, 0)
	for _, descriptor := range sourcecatalog.List() {
		if descriptor.LiveMessages || descriptor.Drafts || descriptor.Calendars {
			supported = append(supported, descriptor.Type)
		}
	}
	for _, source := range sources {
		if !slices.Contains(supported, source) {
			return fmt.Errorf("invalid [mcp.events] sources: unknown source type %q; supported: %s", source, strings.Join(supported, ", "))
		}
	}
	return nil
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
func schemaResourceReference() map[string]any {
	return map[string]any{schemaTypeProperty: "string", "pattern": `^mcp1\.[A-Za-z0-9_-]{42}[AEIMQUYcgkosw048]$`}
}
func schemaNullable(kind string) map[string]any {
	return map[string]any{schemaTypeProperty: []string{kind, "null"}}
}
func closedObject(properties map[string]any, required []string) map[string]any {
	return map[string]any{schemaTypeProperty: "object", "properties": properties, "required": required, "additionalProperties": false}
}
func (s *Service) Catalog() ListResult {
	result := ListResult{Events: make([]Definition, 0)}
	for _, family := range []string{messageFamily, sourceMessageFamily, inboxMessageFamily, calendarFamily, draftFamily} {
		var kinds, draftKinds []string
		var messageAttachments, matrixMessages bool
		for _, cap := range s.caps {
			if cap.Family == family {
				kinds = append(kinds, cap.Kinds...)
				if family == messageFamily || family == sourceMessageFamily || family == inboxMessageFamily {
					if cap.SourceType == "matrix" {
						matrixMessages = true
					} else {
						messageAttachments = true
					}
				}
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
		props := map[string]any{"conversation_id": schemaID(), "conversation_ref": schemaResourceReference()}
		required := []string{"conversation_id"}
		payload := map[string]any{"kind": map[string]any{schemaTypeProperty: "string", "enum": kinds}, "conversation_id": schemaID(), "source_id": schemaID()}
		payloadRequired := []string{"kind", "conversation_id", "source_id"}
		description := "A newly archived message in one conversation. Read content with get_message and list_thread."
		if messageAttachments {
			description += " Read available archived attachments with get_attachment"
			if matrixMessages {
				description += " (not available for Matrix)"
			}
			description += "."
		}
		description += " get_mcp_event recovers a lost payload from eventId."
		switch family {
		case messageFamily, sourceMessageFamily, inboxMessageFamily:
			props["include_from_me"] = map[string]any{schemaTypeProperty: "boolean", "default": false}
			payload["from_me"] = map[string]any{schemaTypeProperty: "boolean"}
			payload["message_id"] = schemaID()
			payload["sent_at"] = schemaNullable("string")
			payload["archived_at"] = schemaString()
			payloadRequired = append(payloadRequired, "from_me", "message_id", "sent_at", "archived_at")
			if family == sourceMessageFamily {
				props = map[string]any{"source_id": schemaID(), "source_ref": schemaResourceReference(), "include_from_me": map[string]any{schemaTypeProperty: "boolean", "default": false}, "include_reactions": map[string]any{schemaTypeProperty: "boolean", "default": true}}
				required = []string{"source_id", "source_ref"}
				description = "A newly archived message in one native source, including conversations created later. Read content with get_message and list_thread. get_mcp_event recovers a lost payload from eventId."
			}
			if family == inboxMessageFamily {
				props = map[string]any{"inbox_id": schemaID(), "inbox_ref": schemaResourceReference(), "include_from_me": map[string]any{schemaTypeProperty: "boolean", "default": false}, "include_reactions": map[string]any{schemaTypeProperty: "boolean", "default": true}}
				required = []string{"inbox_id", "inbox_ref"}
				payload["inbox_id"] = schemaID()
				payloadRequired = append(payloadRequired, "inbox_id")
				description = "A newly archived message observed in one exact native inbox. Read content with get_message and list_thread. get_mcp_event recovers a lost payload from eventId."
			}
		case calendarFamily:
			props = map[string]any{"calendar_source_id": schemaID(), "source_ref": schemaResourceReference()}
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
