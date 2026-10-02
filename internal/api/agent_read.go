package api

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

// Every admitted read has either a source filter or an object-source check.
// SQL, export, administrative reads, and unsupported analytics remain owner-only.
var agentReadPermissions = map[string]agentgrant.Permission{
	"searchCLI": agentgrant.PermissionSearchRead, "searchMessages": agentgrant.PermissionSearchRead,
	"fastSearch": agentgrant.PermissionSearchRead, "deepSearch": agentgrant.PermissionSearchRead,
	"getAggregates": agentgrant.PermissionSearchRead, "getSubAggregates": agentgrant.PermissionSearchRead,
	"filterMessages": agentgrant.PermissionSearchRead, "listMessages": agentgrant.PermissionSearchRead,
	"searchMessagesByDomains": agentgrant.PermissionSearchRead,
	"getStats":                agentgrant.PermissionStatsRead, "getCLIStats": agentgrant.PermissionStatsRead, "getTotalStats": agentgrant.PermissionStatsRead,
	"getCLIMessageThread": agentgrant.PermissionMessageRead,
	"getMessage":          agentgrant.PermissionMessageRead, "getCLIMessage": agentgrant.PermissionMessageRead,
	"getAttachment": agentgrant.PermissionAttachmentRead, "getAttachmentContent": agentgrant.PermissionAttachmentRead,
	"getCLIAttachment": agentgrant.PermissionAttachmentRead,
	"listCLIAccounts":  agentgrant.PermissionSearchRead, "listCLICollections": agentgrant.PermissionSearchRead,
}

func agentReadDenied(permission agentgrant.Permission) *apiHTTPError {
	return newAPIHTTPError(http.StatusUnauthorized, "permission_denied", "Agent grant requires "+string(permission)+" on the requested sources")
}

func (s *Server) agentMessageSourceAllowed(r *http.Request, sourceID int64) bool {
	grant := s.requestAuthentication(r).Grant
	if grant == nil {
		return true
	}
	resolver, ok := s.store.(agentGrantSourceResolver)
	if !ok {
		return false
	}
	source, err := resolver.GetSourceByIDContext(r.Context(), sourceID)
	return err == nil && source != nil && grant.Allows(agentgrant.PermissionMessageRead, agentgrant.SourceRef{Type: source.SourceType, Identifier: source.Identifier})
}

type agentReadSources interface {
	ListSourcesContext(ctx context.Context, sourceType string) ([]*store.Source, error)
	GetMessageSourceContext(ctx context.Context, id int64) (*store.Source, error)
}

func (s *Server) authorizeAgentRead(r *http.Request, operation string, grant *agentgrant.Grant) *apiHTTPError {
	permission, read := agentReadPermissions[operation]
	if !read {
		return nil
	} // runCLI has its own command authorization; health discloses no archive data.
	if operation == "listCLIAccounts" || operation == "listCLICollections" {
		permission = agentDiscoveryPermission(grant)
		if permission == "" {
			return newAPIHTTPError(401, "permission_denied", "Agent grant requires search.read, message.read, attachment.read, or stats.read on the requested sources")
		}
	}
	if grant == nil || permission == "" || !grant.HasPermission(permission) {
		return agentReadDenied(permission)
	}
	resolver, ok := s.store.(agentReadSources)
	if !ok {
		return newAPIHTTPError(503, "source_scope_unavailable", "Source authorization is unavailable")
	}
	sources, err := resolver.ListSourcesContext(r.Context(), "")
	if err != nil {
		return newAPIHTTPError(503, "source_scope_unavailable", "Source authorization is unavailable")
	}
	allowed := make([]int64, 0)
	for _, src := range sources {
		if grant.Allows(permission, agentgrant.SourceRef{Type: src.SourceType, Identifier: src.Identifier}) {
			allowed = append(allowed, src.ID)
		}
	}
	objectAllowed := func(id int64) bool {
		src, err := resolver.GetMessageSourceContext(r.Context(), id)
		return err == nil && src != nil && slices.Contains(allowed, src.ID)
	}
	switch operation {
	case "getCLIMessageThread":
		return nil // the thread reader checks its resolved containing source before responding.
	case "getMessage":
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || !objectAllowed(id) {
			return agentReadDenied(permission)
		}
		return nil
	case "getCLIMessage":
		if s.queryEngineForContext(r.Context()) == nil {
			return newAPIHTTPError(503, "engine_unavailable", "Query engine not available")
		}
		msg, err := s.resolveCLIMessage(r, r.URL.Query().Get("id"))
		if err != nil || msg == nil || !objectAllowed(msg.ID) {
			return agentReadDenied(permission)
		}
		return nil
	case "getAttachment", "getCLIAttachment", "getAttachmentContent":
		attachments, ok := s.store.(interface {
			AgentAttachmentSourceIDsContext(ctx context.Context, id int64, hash string) ([]int64, error)
		})
		if !ok {
			return newAPIHTTPError(503, "source_scope_unavailable", "Attachment authorization is unavailable")
		}
		raw := r.PathValue("id")
		id, _ := strconv.ParseInt(raw, 10, 64)
		hash := r.PathValue("hash")
		if operation == "getCLIAttachment" {
			hash = r.URL.Query().Get("content_hash")
		}
		ids, err := attachments.AgentAttachmentSourceIDsContext(r.Context(), id, hash)
		if err == nil {
			for _, sourceID := range ids {
				if slices.Contains(allowed, sourceID) {
					return nil
				}
			}
		}
		return agentReadDenied(permission)
	case "listCLIAccounts", "listCLICollections":
		return nil // discovery handlers filter their projections by current durable identity.
	}
	requested := []int64(nil)
	q := r.URL.Query()
	if q.Get("account") != "" || q.Get("collection") != "" {
		st, apiErr := s.cliStore()
		if apiErr != nil {
			return apiErr
		}
		scope, err := resolveCLIStatsScope(bindCLIStoreContext(r.Context(), st), q.Get("account"), q.Get("collection"))
		if err != nil {
			return agentReadDenied(permission)
		}
		requested = scope.sourceIDs()
		if requested == nil {
			requested = []int64{}
		}
	}
	for _, name := range []string{"source_id", "source_ids"} {
		ids, present, err := queryInt64s(r, name)
		if err != nil {
			return newAPIHTTPError(400, "invalid_source", err.Error())
		}
		if present {
			if requested == nil {
				requested = ids
			} else {
				for _, id := range ids {
					if !slices.Contains(requested, id) {
						return agentReadDenied(permission)
					}
				}
				requested = ids
			}
		}
	}
	parsed := search.Parse(q.Get("q"))
	if err := parsed.Err(); err != nil {
		return newAPIHTTPError(400, "invalid_query", err.Error())
	}
	for _, id := range parsed.AccountIDs {
		if !slices.Contains(allowed, id) {
			return agentReadDenied(permission)
		}
	}
	if requested == nil && len(parsed.AccountIDs) > 0 {
		requested = parsed.AccountIDs
	}
	if requested == nil {
		requested = allowed
	}
	for _, id := range requested {
		if !slices.Contains(allowed, id) {
			return agentReadDenied(permission)
		}
	}
	if mode := q.Get("mode"); operation == "searchMessages" && mode != "" && mode != "fts" {
		return newAPIHTTPError(400, "unsupported_agent_scope", "Agent search supports fts mode only")
	}
	q.Del("account")
	q.Del("collection")
	q.Del("source_id")
	q.Del("source_ids")
	if len(requested) == 0 {
		requested = []int64{-1}
	} // an explicit empty population must never become unrestricted.
	if operation == "deepSearch" {
		if len(requested) != 1 {
			return newAPIHTTPError(400, "unsupported_agent_scope", "Deep search requires exactly one granted source")
		}
		q.Set("source_id", strconv.FormatInt(requested[0], 10))
	} else {
		ids := make([]string, len(requested))
		for i, id := range requested {
			ids[i] = strconv.FormatInt(id, 10)
		}
		q.Set("source_ids", strings.Join(ids, ","))
	}
	r.URL.RawQuery = q.Encode()
	return nil
}

func agentSourceVisible(grant *agentgrant.Grant, source *store.Source) bool {
	return grant == nil || grant.Allows(agentDiscoveryPermission(grant), agentgrant.SourceRef{Type: source.SourceType, Identifier: source.Identifier})
}

func agentReadSourceIDs(r *http.Request) []int64 {
	ids, _, _ := queryInt64s(r, "source_ids")
	return ids
}

func (s *Server) agentScopedStats(w http.ResponseWriter, r *http.Request, cli bool) {
	st, apiErr := s.cliStore()
	if apiErr != nil {
		writeAPIHTTPError(w, apiErr)
		return
	}
	ids := agentReadSourceIDs(r)
	stats, err := getCLIStatsForScope(r.Context(), bindCLIStoreContext(r.Context(), st), ids)
	if err != nil {
		writeError(w, 500, "stats_failed", "Could not retrieve scoped statistics")
		return
	}
	// Archive file size is global, so scoped grants do not disclose it.
	stats.DatabaseSize = 0
	resp := statsResponseFromStore(stats)
	if cli {
		writeJSON(w, 200, cliStatsResponse{Stats: resp, ScopeLabel: "agent grant", ScopeSourceCount: len(ids)})
	} else {
		writeJSON(w, 200, resp)
	}
}

func agentDiscoveryPermission(grant *agentgrant.Grant) agentgrant.Permission {
	if grant != nil {
		for _, p := range []agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionMessageRead, agentgrant.PermissionAttachmentRead, agentgrant.PermissionStatsRead} {
			if grant.HasPermission(p) {
				return p
			}
		}
	}
	return ""
}

func (s *Server) agentAttachmentVisible(r *http.Request, grant *agentgrant.Grant, id int64) bool {
	attachments, ok := s.store.(interface {
		AgentAttachmentSourceIDsContext(ctx context.Context, id int64, hash string) ([]int64, error)
	})
	resolver, sourceOK := s.store.(agentGrantSourceResolver)
	if !ok || !sourceOK {
		return false
	}
	ids, err := attachments.AgentAttachmentSourceIDsContext(r.Context(), id, "")
	if err != nil {
		return false
	}
	for _, id := range ids {
		source, err := resolver.GetSourceByIDContext(r.Context(), id)
		if err == nil && source != nil && grant.Allows(agentgrant.PermissionAttachmentRead, agentgrant.SourceRef{Type: source.SourceType, Identifier: source.Identifier}) {
			return true
		}
	}
	return false
}

func (s *Server) handleAgentListMessages(w http.ResponseWriter, r *http.Request) {
	engine := s.queryEngineForContext(r.Context())
	if engine == nil {
		writeError(w, 503, "engine_unavailable", "Query engine not available")
		return
	}
	page, _, err := queryInt(r, "page")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	page = max(page, 1)
	size, present, err := queryInt(r, "page_size")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if !present || size < 1 {
		size = 20
	}
	size = min(size, 100)
	filter := query.MessageFilter{SourceIDs: agentReadSourceIDs(r), Pagination: query.Pagination{Limit: size, Offset: (page - 1) * size}}
	messages, err := engine.ListMessages(r.Context(), filter)
	if err != nil {
		writeError(w, 500, "message_query_failed", "Could not list scoped messages")
		return
	}
	stats, err := engine.GetTotalStats(r.Context(), query.StatsOptions{Filter: &filter, SourceIDs: filter.SourceIDs})
	if err != nil {
		writeError(w, 500, "message_query_failed", "Could not count scoped messages")
		return
	}
	summaries := make([]MessageSummary, len(messages))
	for i, msg := range messages {
		summaries[i] = toMessageSummaryFromQuery(msg)
	}
	writeJSON(w, 200, MessageListResponse{Total: stats.MessageCount, Page: page, PageSize: size, Messages: summaries})
}
