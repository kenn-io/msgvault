package mcpevents

import (
	"context"
	"slices"
	"strconv"

	"go.kenn.io/msgvault/internal/sourcecatalog"
	"go.kenn.io/msgvault/internal/store"
)

// ResourceQuery requests a bounded live view of archive resource identities.
// The principal is supplied separately by the authenticated daemon boundary.
type ResourceQuery struct {
	Kind     string `json:"kind" enum:"source,conversation,inbox"`
	SourceID string `json:"source_id,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
	Limit    int    `json:"limit,omitempty" minimum:"0" maximum:"100"`
}

type ResourceCapabilities struct {
	Events            []string `json:"events"`
	Read              []string `json:"read"`
	Transport         string   `json:"transport"`
	UnavailableReason string   `json:"unavailable_reason,omitempty"`
}

type Resource struct {
	ScopeKind        string               `json:"scope_kind,omitempty"`
	ParentProviderID string               `json:"parent_provider_id,omitempty"`
	NativeEpoch      string               `json:"native_epoch,omitempty"`
	Kind             string               `json:"kind"`
	ID               string               `json:"id"`
	SourceID         string               `json:"source_id"`
	SourceType       string               `json:"source_type"`
	ProviderID       string               `json:"provider_id"`
	DisplayName      string               `json:"display_name"`
	Reference        string               `json:"reference"`
	Capabilities     ResourceCapabilities `json:"capabilities"`
}

type ResourcePage struct {
	Resources  []Resource `json:"resources"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// DiscoverResources is owner-only even when event capture is disabled. It
// returns safe native metadata and never contacts a provider or receiver.
func (s *Service) DiscoverResources(ctx context.Context, principal string, query ResourceQuery) (ResourcePage, error) {
	result := ResourcePage{Resources: make([]Resource, 0)}
	if s.opts.OwnerKey == "" || principal != s.principal {
		return result, &Error{Code: -32012, Reason: "owner_required"}
	}
	if query.Kind != "source" && query.Kind != "conversation" && query.Kind != "inbox" || query.Limit < 0 || query.Limit > 100 {
		return result, invalid("invalid_resource_query")
	}
	var sourceID int64
	if query.SourceID != "" {
		var err error
		sourceID, err = decimal(query.SourceID)
		if err != nil || sourceID == 0 {
			return result, invalid("invalid_resource_query")
		}
	}
	err := s.operation(ctx, func() error {
		page, err := s.st.ListMCPResourcePage(ctx, store.MCPResourcePageOptions{Principal: principal, Kind: query.Kind, SourceID: sourceID, Cursor: query.Cursor, Limit: query.Limit})
		if err != nil {
			return err
		}
		for _, row := range page.Resources {
			capabilities, err := s.resourceCapabilities(ctx, row)
			if err != nil {
				return err
			}
			result.Resources = append(result.Resources, Resource{ScopeKind: row.ScopeKind, ParentProviderID: row.ParentProviderID, NativeEpoch: row.Epoch, Kind: row.Kind, ID: strconv.FormatInt(row.ID, 10), SourceID: strconv.FormatInt(row.SourceID, 10), SourceType: row.SourceType, ProviderID: row.ProviderID, DisplayName: row.DisplayName, Reference: row.Reference, Capabilities: capabilities})
		}
		result.NextCursor = page.NextCursor
		return nil
	})
	if err != nil {
		switch err.Error() {
		case "invalid_resource_query", "invalid_discovery_cursor", "resource_unavailable":
			return ResourcePage{}, invalid(err.Error())
		default:
			return ResourcePage{}, safeStoreError(err)
		}
	}
	return result, nil
}

func (s *Service) resourceCapabilities(ctx context.Context, row store.MCPResource) (ResourceCapabilities, error) {
	result := ResourceCapabilities{Events: []string{}, Read: resourceReadTools(row.Kind, row.SourceType), Transport: "unknown"}
	if descriptor, ok := sourcecatalog.Lookup(row.SourceType); ok {
		result.Transport = descriptor.Transport
	}
	if !s.opts.Enabled {
		result.UnavailableReason = "events_disabled"
		return result, nil
	}
	if row.SourceType == sourcecatalog.Gmail || row.SourceType == sourcecatalog.MicrosoftMail || row.SourceType == sourcecatalog.IMAP || row.SourceType == sourcecatalog.Slack || row.SourceType == sourcecatalog.Teams || row.SourceType == sourcecatalog.Discord || row.SourceType == sourcecatalog.Matrix || row.SourceType == sourcecatalog.Beeper {
		allowed, err := s.st.NativeSourceMessagePermission(ctx, row.SourceID)
		if err != nil {
			return result, err
		}
		if !allowed {
			result.UnavailableReason = "native_authority_unavailable"
			return result, nil
		}
	}
	result.UnavailableReason = "source_not_selected"
	for _, capability := range s.caps {
		if capability.SourceType != row.SourceType {
			continue
		}
		result.UnavailableReason = "scope_not_supported"
		if !resourceSupportsFamily(row.Kind, capability.Family) {
			continue
		}
		_, _, err := s.st.ValidateMCPEventScope(ctx, capability.Family, row.Kind, row.ID, capability.Kinds)
		if err != nil {
			switch err.Error() {
			case "native_authority_unavailable":
				result.UnavailableReason = "native_authority_unavailable"
				continue
			case "unknown_scope", "wrong_scope_type", "source_not_capable", "kind_not_capable":
				result.UnavailableReason = "scope_not_supported"
				continue
			default:
				return result, err
			}
		}
		if !slices.Contains(result.Events, capability.Family) {
			result.Events = append(result.Events, capability.Family)
		}
	}
	if len(result.Events) > 0 {
		result.UnavailableReason = ""
		slices.Sort(result.Events)
	}
	return result, nil
}

func resourceReadTools(kind, sourceType string) []string {
	if sourceType == sourcecatalog.CardDAV {
		return []string{"get_person_profile"}
	}
	result := make([]string, 0)
	for _, capability := range capabilities(true, []string{sourceType}) {
		if !resourceSupportsFamily(kind, capability.Family) {
			continue
		}
		for _, tool := range ReadToolsForCapability(capability.Family, sourceType) {
			if !slices.Contains(result, tool) {
				result = append(result, tool)
			}
		}
	}
	return result
}

func resourceSupportsFamily(kind, family string) bool {
	switch kind {
	case "source":
		return family == calendarFamily || family == sourceMessageFamily
	case "conversation":
		return family == messageFamily || family == draftFamily
	case "inbox":
		return family == inboxMessageFamily
	default:
		return false
	}
}
