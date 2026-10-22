package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"strconv"
	"strings"
)

// MCPResource exposes native identity and presentation without configuration or
// private lifetime keys. Wire adapters render its integer IDs as decimal strings.
type MCPResource struct {
	Kind                                           string
	ID, SourceID                                   int64
	SourceType, ProviderID, DisplayName, Reference string
	ScopeKind, ParentProviderID, Epoch             string
}

// CanonicalMCPResourceReference reports whether a reference has its exact wire
// encoding. It does not authenticate the reference or authorize a resource.
func CanonicalMCPResourceReference(reference string) bool {
	if len(reference) != 48 || !strings.HasPrefix(reference, "mcp1.") {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(reference[5:])
	return err == nil && len(raw) == sha256.Size && base64.RawURLEncoding.EncodeToString(raw) == reference[5:]
}

func mcpResourceMAC(generation string, fields []string) (string, error) {
	key, err := hex.DecodeString(generation)
	if err != nil || len(key) != 32 {
		return "", mcpStoreError("resource_unavailable")
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func mcpResourceReference(resource MCPResource, generation, parent string) (string, error) {
	mac, err := mcpResourceMAC(generation, []string{"msgvault/resource/v1", resource.Kind, strconv.FormatInt(resource.ID, 10), strconv.FormatInt(resource.SourceID, 10), resource.SourceType, resource.ProviderID, parent})
	if err != nil {
		return "", err
	}
	return "mcp1." + mac, nil
}

func (s *Store) mcpResource(ctx context.Context, tx *sql.Tx, kind string, id int64, create bool) (MCPResource, mcpScopeLifetime, error) {
	resource := MCPResource{Kind: kind, ID: id}
	var lifetime mcpScopeLifetime
	if id <= 0 {
		return resource, lifetime, mcpStoreError("invalid_resource_reference")
	}
	var identifier string
	var err error
	switch kind {
	case mcpScopeSource:
		resource.SourceID = id
		// #nosec G701 -- Fixed SQL; the exact archive source identity is bound.
		err = tx.QueryRowContext(ctx, s.Rebind(`SELECT source_type,identifier,COALESCE(display_name,'') FROM sources WHERE id=?`), id).Scan(&resource.SourceType, &resource.ProviderID, &resource.DisplayName)
	case mcpScopeConversation:
		// #nosec G701 -- Fixed SQL; the exact archive resource identity is bound.
		err = tx.QueryRowContext(ctx, s.Rebind(`SELECT c.source_id,s.source_type,COALESCE(c.source_conversation_id,''),COALESCE(c.title,''),s.identifier FROM conversations c JOIN sources s ON s.id=c.source_id WHERE c.id=?`), id).Scan(&resource.SourceID, &resource.SourceType, &resource.ProviderID, &resource.DisplayName, &identifier)
	case mcpScopeInbox:
		// #nosec G701 -- Fixed SQL; the exact archive resource identity is bound.
		err = tx.QueryRowContext(ctx, s.Rebind(`SELECT n.source_id,s.source_type,n.provider_id,n.display_name,s.identifier,n.scope_kind,n.parent_provider_id,n.native_epoch FROM native_message_scopes n JOIN sources s ON s.id=n.source_id WHERE n.id=? AND n.present`), id).Scan(&resource.SourceID, &resource.SourceType, &resource.ProviderID, &resource.DisplayName, &identifier, &resource.ScopeKind, &resource.ParentProviderID, &resource.Epoch)
	default:
		return resource, lifetime, mcpStoreError("invalid_resource_reference")
	}
	if errors.Is(err, sql.ErrNoRows) {
		return resource, lifetime, mcpStoreError("resource_unavailable")
	}
	if err != nil {
		return resource, lifetime, err
	}
	if create {
		lifetime, err = s.ensureMCPScopeLifetime(ctx, tx, kind, id, resource.SourceID)
	} else {
		lifetime, err = s.mcpScopeLifetime(ctx, tx, kind, id, resource.SourceID)
	}
	if err != nil {
		return resource, lifetime, err
	}
	var parent string
	if kind == mcpScopeConversation || kind == mcpScopeInbox {
		source := MCPResource{Kind: mcpScopeSource, ID: resource.SourceID, SourceID: resource.SourceID, SourceType: resource.SourceType, ProviderID: identifier}
		parent, err = mcpResourceReference(source, lifetime.source, "")
		if err != nil {
			return resource, lifetime, err
		}
	}
	resource.Reference, err = mcpResourceReference(resource, lifetime.scope, parent)
	return resource, lifetime, err
}

// GetMCPResource resolves one native resource under the identity/source/Events
// fences, creating its private lifetime only when missing. It does no provider IO.
func (s *Store) GetMCPResource(ctx context.Context, kind string, id int64) (MCPResource, error) {
	var resource MCPResource
	err := s.withMCPEventTx(ctx, func(tx *sql.Tx, _ MCPEventClock) error {
		var err error
		resource, _, err = s.mcpResource(ctx, tx, kind, id, true)
		return err
	})
	return resource, err
}

// MCPResourcePageOptions describes a bounded live view. Principal is supplied by
// the authenticated owner adapter, never copied from a client request body.
type MCPResourcePageOptions struct {
	Principal, Kind, Cursor string
	SourceID                int64
	Limit                   int
}

type MCPResourcePage struct {
	Resources  []MCPResource
	NextCursor string
}

func mcpResourcePageMAC(opts MCPResourcePageOptions, id int64, lifetime mcpScopeLifetime) (string, error) {
	return mcpResourceMAC(lifetime.scope, []string{"msgvault/resource-page/v1", opts.Principal, opts.Kind, strconv.FormatInt(opts.SourceID, 10), strconv.Itoa(opts.Limit), strconv.FormatInt(id, 10), lifetime.source})
}

func (s *Store) mcpResourcePageAnchor(ctx context.Context, tx *sql.Tx, opts MCPResourcePageOptions) (int64, error) {
	if opts.Cursor == "" {
		return 0, nil
	}
	if len(opts.Cursor) > 70 || !strings.HasPrefix(opts.Cursor, "mcpp1.") {
		return 0, mcpStoreError("invalid_discovery_cursor")
	}
	idText, mac, ok := strings.Cut(opts.Cursor[6:], ".")
	id, err := strconv.ParseInt(idText, 10, 64)
	if !ok || err != nil || id <= 0 || strconv.FormatInt(id, 10) != idText {
		return 0, mcpStoreError("invalid_discovery_cursor")
	}
	_, lifetime, err := s.mcpResource(ctx, tx, opts.Kind, id, false)
	if errors.Is(err, mcpStoreError("resource_unavailable")) {
		return 0, mcpStoreError("invalid_discovery_cursor")
	}
	if err != nil {
		return 0, err
	}
	expected, err := mcpResourcePageMAC(opts, id, lifetime)
	if err != nil {
		return 0, err
	}
	if !hmac.Equal([]byte(expected), []byte(mac)) {
		return 0, mcpStoreError("invalid_discovery_cursor")
	}
	return id, nil
}

// ListMCPResourcePage returns at most 100 native resources (25 by default).
// Pages are live keyset reads, not a snapshot across requests. Cursors bind the
// owner and filters; a removed/rebound anchor requires a fresh traversal.
func (s *Store) ListMCPResourcePage(ctx context.Context, opts MCPResourcePageOptions) (MCPResourcePage, error) {
	result := MCPResourcePage{Resources: make([]MCPResource, 0)}
	if opts.Limit == 0 {
		opts.Limit = 25
	}
	if opts.Principal == "" {
		return result, mcpStoreError("owner_required")
	}
	if opts.Limit < 1 || opts.Limit > 100 || opts.SourceID < 0 || opts.Kind != mcpScopeSource && opts.Kind != mcpScopeConversation && opts.Kind != mcpScopeInbox {
		return result, mcpStoreError("invalid_resource_query")
	}
	err := s.withMCPEventTx(ctx, func(tx *sql.Tx, _ MCPEventClock) error {
		after, err := s.mcpResourcePageAnchor(ctx, tx, opts)
		if err != nil {
			return err
		}
		query := `SELECT id FROM sources WHERE id>? ORDER BY id LIMIT ?`
		args := []any{after, opts.Limit + 1}
		if opts.Kind == mcpScopeConversation {
			query = `SELECT id FROM conversations WHERE id>? ORDER BY id LIMIT ?`
		}
		if opts.SourceID > 0 {
			query = `SELECT id FROM sources WHERE id>? AND id=? ORDER BY id LIMIT ?`
			if opts.Kind == mcpScopeConversation {
				query = `SELECT id FROM conversations WHERE id>? AND source_id=? ORDER BY id LIMIT ?`
			}
			args = []any{after, opts.SourceID, opts.Limit + 1}
		}
		if opts.Kind == mcpScopeInbox {
			query = `SELECT n.id FROM native_message_scopes n WHERE n.id>? AND n.present AND EXISTS (SELECT 1 FROM mcp_source_resources sr WHERE sr.source_id=n.source_id AND sr.generation=n.source_generation) ORDER BY n.id LIMIT ?`
			args = []any{after, opts.Limit + 1}
			if opts.SourceID > 0 {
				query = `SELECT n.id FROM native_message_scopes n WHERE n.id>? AND n.source_id=? AND n.present AND EXISTS (SELECT 1 FROM mcp_source_resources sr WHERE sr.source_id=n.source_id AND sr.generation=n.source_generation) ORDER BY n.id LIMIT ?`
				args = []any{after, opts.SourceID, opts.Limit + 1}
			}
		}
		// #nosec G701 -- Query is one of six fixed statements; all client values are bound.
		rows, err := tx.QueryContext(ctx, s.Rebind(query), args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		ids := make([]int64, 0, opts.Limit+1)
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		more := len(ids) > opts.Limit
		if more {
			ids = ids[:opts.Limit]
		}
		for _, id := range ids {
			resource, lifetime, err := s.mcpResource(ctx, tx, opts.Kind, id, true)
			if err != nil {
				return err
			}
			result.Resources = append(result.Resources, resource)
			if more && len(result.Resources) == len(ids) {
				mac, err := mcpResourcePageMAC(opts, id, lifetime)
				if err != nil {
					return err
				}
				result.NextCursor = "mcpp1." + strconv.FormatInt(id, 10) + "." + mac
			}
		}
		return nil
	})
	return result, err
}
