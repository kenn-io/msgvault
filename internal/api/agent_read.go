package api

import (
	"context"
	"database/sql"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/query"
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
	return newAPIHTTPError(http.StatusForbidden, "permission_denied", "Agent grant requires "+string(permission)+" on the requested sources")
}

func (s *Server) agentObjectSourceAllowed(r *http.Request, sourceID int64) bool {
	return s.requestAuthentication(r).Grant == nil || slices.Contains(agentReadSourceIDs(r), sourceID)
}

func agentReadPermission(operation string, grant *agentgrant.Grant) (agentgrant.Permission, *apiHTTPError) {
	permission, read := agentReadPermissions[operation]
	if !read {
		return "", nil
	} // runCLI has its own command authorization; health discloses no archive data.
	if operation == "listCLIAccounts" || operation == "listCLICollections" {
		permission = agentDiscoveryPermission(grant)
		if permission == "" {
			return "", newAPIHTTPError(http.StatusForbidden, "permission_denied", "Agent grant requires search.read, message.read, attachment.read, or stats.read on the requested sources")
		}
		return permission, nil
	}
	if grant == nil || permission == "" || !grant.HasPermission(permission) {
		return "", agentReadDenied(permission)
	}
	return permission, nil
}

// agentReadConcurrency stays below the SQLite pool size (4) because each agent
// read holds one connection for its snapshot; sync writes and owner reads keep the rest.
const agentReadConcurrency = 2

type agentReadSnapshotStore interface {
	BeginReadSnapshotContext(ctx context.Context) (context.Context, func(), error)
	DB() *sql.DB
	IsPostgreSQL() bool
}

// beginAgentRead binds req to one read snapshot and authorizes its source scope.
// The returned release frees the snapshot and its concurrency slot.
func (s *Server) beginAgentRead(req *http.Request, operation string, grant *agentgrant.Grant, permission agentgrant.Permission) (func(), *apiHTTPError) {
	snapshotStore, ok := s.store.(agentReadSnapshotStore)
	if !ok {
		return nil, newAPIHTTPError(http.StatusServiceUnavailable, "source_scope_unavailable", "Source authorization is unavailable")
	}
	select {
	case s.agentReadSlots <- struct{}{}:
	case <-req.Context().Done():
		return nil, newAPIHTTPError(http.StatusServiceUnavailable, "agent_read_busy", "Agent reads are at capacity; retry later")
	}
	indexGeneration := s.ftsRebuildGen.Load()
	indexComplete := s.ftsIndexComplete.Load()
	readContext, releaseSnapshot, err := snapshotStore.BeginReadSnapshotContext(req.Context())
	if err != nil {
		<-s.agentReadSlots
		return nil, newAPIHTTPError(http.StatusServiceUnavailable, "source_scope_unavailable", "Source authorization is unavailable")
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			releaseSnapshot()
			<-s.agentReadSlots
		})
	}
	if boundary, ok := req.Context().Value(readResponseKey{}).(*readResponseWriter); ok {
		boundary.release = release
	}
	if s.queryEngineForContext(readContext) != nil {
		engine := query.NewEngine(snapshotStore.DB(), snapshotStore.IsPostgreSQL())
		readContext = context.WithValue(readContext, analyticsEngineContextKey{}, &analyticsEngineState{engine: engine, mode: AnalyticsModeSQL})
	}
	*req = *req.WithContext(readContext)
	if apiErr := s.authorizeAgentRead(req, operation, grant, permission, indexGeneration, indexComplete); apiErr != nil {
		release()
		return nil, apiErr
	}
	return release, nil
}

type agentReadScopeKey struct{}
type agentReadScope struct {
	ids        []int64
	scope      cliScope
	indexState string
}

func (s agentReadScope) sourceCount() int {
	count := 0
	for _, id := range s.ids {
		if id > 0 {
			count++
		}
	}
	return count
}

func (s *Server) authorizeAgentRead(r *http.Request, operation string, grant *agentgrant.Grant, permission agentgrant.Permission, indexGeneration uint64, indexComplete bool) *apiHTTPError {
	resolver, ok := s.store.(operationSourceLister)
	if !ok {
		return newAPIHTTPError(503, "source_scope_unavailable", "Source authorization is unavailable")
	}
	sources, err := resolver.ListSourcesContext(r.Context(), "")
	if err != nil {
		return newAPIHTTPError(503, "source_scope_unavailable", "Source authorization is unavailable")
	}
	allowed := make([]int64, 0)
	account := r.URL.Query().Get("account")
	accountGranted := false
	for _, src := range sources {
		if grant.MatchesSource(agentgrant.SourceRef{Type: src.SourceType, Identifier: src.Identifier}) {
			allowed = append(allowed, src.ID)
			accountGranted = accountGranted || src.Identifier == account
		}
	}
	*r = *r.WithContext(context.WithValue(r.Context(), agentReadScopeKey{}, agentReadScope{ids: allowed}))
	switch operation {
	case "getCLIMessageThread":
		if account != "" && !accountGranted {
			return agentReadDenied(permission)
		}
		return nil
	case "getMessage", "getCLIMessage", "getAttachment", "getAttachmentContent", "getCLIAttachment", "listCLIAccounts", "listCLICollections":
		return nil
	}
	requested := []int64(nil)
	var resolved cliScope
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
		resolved = scope
		requested = scope.sourceIDs()
		if requested == nil {
			requested = []int64{}
		}
		// Every member must be granted, even when source_ids narrows the scope below.
		for _, id := range requested {
			if !slices.Contains(allowed, id) {
				return agentReadDenied(permission)
			}
		}
	}
	single, hasSingle, err := queryInt64(r, "source_id")
	if err != nil {
		return newAPIHTTPError(400, "invalid_source", err.Error())
	}
	ids, hasMultiple, err := queryInt64s(r, "source_ids")
	if err != nil {
		return newAPIHTTPError(400, "invalid_source", err.Error())
	}
	if hasMultiple || hasSingle {
		// Match appendSourceFilter: plural IDs override the validated scalar.
		if !hasMultiple {
			ids = []int64{single}
		}
		if requested != nil {
			for _, id := range ids {
				if !slices.Contains(requested, id) {
					return agentReadDenied(permission)
				}
			}
		}
		requested = ids
	}
	if requested == nil {
		requested = allowed
	}
	requested = normalizeSourceIDs(requested)
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
	*r = *r.WithContext(context.WithValue(r.Context(), agentReadScopeKey{}, agentReadScope{ids: requested, scope: resolved}))
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
	switch operation {
	case "searchCLI", "fastSearch", "deepSearch", "searchMessages", "getAggregates", "getSubAggregates", "filterMessages", "getTotalStats":
		if q.Get("q") != "" || q.Get("search_query") != "" {
			st, apiErr := s.cliStore()
			if apiErr != nil {
				return apiErr
			}
			admitted, _ := r.Context().Value(agentReadScopeKey{}).(agentReadScope)
			admitted.indexState = s.ensureCLISearchIndexAsync(bindCLIStoreContext(r.Context(), st), false)
			if admitted.indexState == "" && (!indexComplete || indexGeneration%2 != 0 || indexGeneration != s.ftsRebuildGen.Load()) {
				admitted.indexState = cliSearchIndexStateUnverified
			}
			*r = *r.WithContext(context.WithValue(r.Context(), agentReadScopeKey{}, admitted))
		}
	}
	return nil
}

func agentReadSourceIDs(r *http.Request) []int64 {
	scope, _ := r.Context().Value(agentReadScopeKey{}).(agentReadScope)
	return scope.ids
}

func (s *Server) agentScopedStats(w http.ResponseWriter, r *http.Request, cli bool) {
	st, apiErr := s.cliStore()
	if apiErr != nil {
		writeAPIHTTPError(w, apiErr)
		return
	}
	scope, _ := r.Context().Value(agentReadScopeKey{}).(agentReadScope)
	if cli && scope.scope.Account.Input != "" && len(scope.scope.sourceIDs()) == 0 {
		collection := ""
		if scope.scope.Collection != nil {
			collection = scope.scope.Collection.Name
		}
		writeError(w, http.StatusBadRequest, "empty_scope", cliEmptyScopeMessage(scope.scope.Account.Input, collection))
		return
	}
	stats, err := getCLIStatsForScope(r.Context(), bindCLIStoreContext(r.Context(), st), scope.ids)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		writeError(w, 500, "stats_failed", "Could not retrieve scoped statistics")
		return
	}
	granted := scope.sourceCount()
	stats.SourceCount = int64(granted)
	// Archive file size is global, so scoped grants do not disclose it.
	resp := statsResponseFromStore(stats)
	resp.DatabaseSize = nil
	if cli {
		label := "agent grant"
		if scope.scope.Account.Input != "" {
			label = scope.scope.displayName()
		}
		writeJSON(w, 200, cliStatsResponse{Stats: resp, ScopeLabel: label, ScopeSourceCount: granted})
	} else {
		writeJSON(w, 200, resp)
	}
}

// agentDiscoveryPermissions lists the read permissions that admit account and
// collection discovery, in a fixed order so denials and logs are stable.
var agentDiscoveryPermissions = []agentgrant.Permission{
	agentgrant.PermissionSearchRead, agentgrant.PermissionMessageRead,
	agentgrant.PermissionAttachmentRead, agentgrant.PermissionStatsRead,
}

func agentDiscoveryPermission(grant *agentgrant.Grant) agentgrant.Permission {
	if grant != nil {
		for _, p := range agentDiscoveryPermissions {
			if grant.HasPermission(p) {
				return p
			}
		}
	}
	return ""
}

func agentIndexState(r *http.Request) string {
	scope, _ := r.Context().Value(agentReadScopeKey{}).(agentReadScope)
	return scope.indexState
}
