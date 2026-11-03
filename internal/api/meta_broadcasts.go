package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/store"
)

// BroadcastReadStore is optional on the API adapter. Older readers fail closed.
type BroadcastReadStore interface {
	ListBroadcastsContext(ctx context.Context, sourceID int64) ([]store.Broadcast, error)
	GetBroadcastContext(ctx context.Context, id string) (store.Broadcast, []store.BroadcastRecipient, error)
	ListMarketingSubscriptionsContext(ctx context.Context, sourceID int64, filter store.SubscriptionFilter) ([]store.MarketingSubscription, error)
	ListSourcesContext(ctx context.Context, sourceType string) ([]*store.Source, error)
	GetSourceByIDContext(ctx context.Context, sourceID int64) (*store.Source, error)
}
type BroadcastSubscriptionView = meta.BroadcastSubscriptionView
type BroadcastSummary = meta.BroadcastSummary
type BroadcastRecipientView = meta.BroadcastRecipientView
type BroadcastDetail = meta.BroadcastDetail
type BroadcastListResponse = meta.BroadcastListResponse
type BroadcastSubscriptionsResponse = meta.BroadcastSubscriptionsResponse

func broadcastTime(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}
	return &v.Time
}
func broadcastInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}
func ProjectBroadcastSubscriptions(rows []store.MarketingSubscription) []BroadcastSubscriptionView {
	out := make([]BroadcastSubscriptionView, 0, len(rows))
	for _, s := range rows {
		clean := func(v string) string {
			if s.Token == "" {
				return v
			}
			return strings.ReplaceAll(v, s.Token, s.Fingerprint)
		}
		out = append(out, BroadcastSubscriptionView{ID: s.ID, SourceID: s.SourceID, ParticipantID: broadcastInt(s.ParticipantID), TokenFingerprint: s.Fingerprint, TopicTitle: clean(s.TopicTitle), Timezone: clean(s.Timezone), Status: s.Status, Reoptin: clean(s.Reoptin), TokenExpiryAt: broadcastTime(s.TokenExpiryAt), NextEligibleAt: broadcastTime(s.NextEligibleAt), LastConfirmedAt: broadcastTime(s.LastConfirmedAt)})
	}
	return out
}
func ProjectBroadcast(b store.Broadcast, page string) BroadcastSummary {
	return BroadcastSummary{ID: b.ID, SourceID: b.SourceID, PageID: page, AdAccountID: b.AdAccountID, Name: b.Name, MessageJSON: b.MessageJSON, Revision: b.Revision, DailyBudgetCents: b.DailyBudgetCents, WorstDailyCents: float64(b.DailyBudgetCents) * 1.75, WorstWeeklyCents: float64(b.DailyBudgetCents) * 7, CampaignDays: b.CampaignDays, MinGapSeconds: b.MinGapSeconds, State: b.State, CampaignID: b.CampaignID, PlanDigest: b.PlanDigest, LastError: b.LastError, CampaignReadyAt: broadcastTime(b.CampaignReadyAt), CampaignEndsAt: broadcastTime(b.CampaignEndsAt), BudgetWarning: b.BudgetWarning, CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt}
}
func ProjectBroadcastDetail(b store.Broadcast, rows []store.BroadcastRecipient, page string) BroadcastDetail {
	out := BroadcastDetail{BroadcastSummary: ProjectBroadcast(b, page), Recipients: make([]BroadcastRecipientView, 0, len(rows))}
	for _, r := range rows {
		out.Recipients = append(out.Recipients, BroadcastRecipientView{SubscriptionID: r.SubscriptionID, ParticipantID: broadcastInt(r.ParticipantID), State: r.State, Reason: r.Reason, ErrorCode: r.ErrorCode, TrackingID: r.TrackingID, MID: r.MID, MessageID: broadcastInt(r.MessageID), AttemptedAt: broadcastTime(r.AttemptedAt), RowRevision: r.RowRevision})
	}
	return out
}
func isBroadcastReadOperation(id string) bool {
	return id == "listBroadcasts" || id == "getBroadcast" || id == "listBroadcastSubscriptions"
}
func (s *Server) registerBroadcastRoutes(api huma.API) {
	list := rawAPIV1Operation("listBroadcasts", http.MethodGet, "/meta/broadcasts", "List a profile's Messenger broadcasts")
	list.Parameters = []*huma.Param{{Name: "profile", In: "query", Required: true, Schema: &huma.Schema{Type: huma.TypeString}}}
	list.Responses = jsonResponsesFor[BroadcastListResponse](api)
	addErrorResponses(api, list.Responses, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, list, s.handleListBroadcasts)
	get := rawAPIV1Operation("getBroadcast", http.MethodGet, "/meta/broadcasts/{id}", "Get a broadcast and recipient receipts")
	get.Parameters = []*huma.Param{{Name: "id", In: "path", Required: true, Schema: &huma.Schema{Type: huma.TypeString}}}
	get.Responses = jsonResponsesFor[BroadcastDetail](api)
	addErrorResponses(api, get.Responses, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, get, s.handleGetBroadcast)
	subs := rawAPIV1Operation("listBroadcastSubscriptions", http.MethodGet, "/meta/broadcast-subscriptions", "List subscription consent with token fingerprints")
	subs.Parameters = []*huma.Param{{Name: "profile", In: "query", Required: true, Schema: &huma.Schema{Type: huma.TypeString}}, {Name: "participant", In: "query", Schema: &huma.Schema{Type: huma.TypeInteger, Format: "int64"}}, {Name: cliRunStatusSubcommand, In: "query", Schema: &huma.Schema{Type: huma.TypeString, Enum: []any{"active", "stopped", "unsubscribed", "absent"}}}}
	subs.Responses = jsonResponsesFor[BroadcastSubscriptionsResponse](api)
	addErrorResponses(api, subs.Responses, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, subs, s.handleBroadcastSubscriptions)
}
func (s *Server) broadcastReader(w http.ResponseWriter) (BroadcastReadStore, bool) {
	reader, ok := s.store.(BroadcastReadStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "feature_unavailable", "Broadcast reads are unavailable")
	}
	return reader, ok
}
func (s *Server) broadcastSource(ctx context.Context, reader BroadcastReadStore, profile string) (*store.Source, *config.MessengerBroadcastProfile, error) {
	if s.cfg == nil {
		return nil, nil, sql.ErrNoRows
	}
	p := s.cfg.GetMessengerBroadcastProfile(profile)
	if p == nil {
		return nil, nil, sql.ErrNoRows
	}
	sources, err := reader.ListSourcesContext(ctx, meta.SourceTypeFacebookMessages)
	if err != nil {
		return nil, nil, err
	}
	id := meta.DMSource{AppID: p.AppID, PageID: p.PageID, Platform: meta.PlatformMessenger}.Identifier()
	for _, source := range sources {
		if source.Identifier == id {
			return source, p, nil
		}
	}
	return nil, nil, sql.ErrNoRows
}
func broadcastReadError(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "Broadcast or profile not found")
	} else {
		writeError(w, http.StatusInternalServerError, "read_failed", "Read broadcasts failed")
	}
}
func (s *Server) handleListBroadcasts(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.broadcastReader(w)
	if !ok {
		return
	}
	if r.URL.Query().Get("profile") == "" {
		writeError(w, http.StatusBadRequest, "invalid_args", "profile is required")
		return
	}
	source, p, err := s.broadcastSource(r.Context(), reader, r.URL.Query().Get("profile"))
	if err != nil {
		broadcastReadError(w, err)
		return
	}
	rows, err := reader.ListBroadcastsContext(r.Context(), source.ID)
	if err != nil {
		broadcastReadError(w, err)
		return
	}
	out := BroadcastListResponse{Broadcasts: make([]BroadcastSummary, 0, len(rows))}
	for _, b := range rows {
		out.Broadcasts = append(out.Broadcasts, ProjectBroadcast(b, p.PageID))
	}
	writeJSON(w, http.StatusOK, out)
}
func (s *Server) handleGetBroadcast(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.broadcastReader(w)
	if !ok {
		return
	}
	b, rows, err := reader.GetBroadcastContext(r.Context(), r.PathValue("id"))
	if err != nil {
		broadcastReadError(w, err)
		return
	}
	source, err := reader.GetSourceByIDContext(r.Context(), b.SourceID)
	if err != nil {
		broadcastReadError(w, err)
		return
	}
	src, valid := meta.ParseDMSourceIdentifier(meta.PlatformMessenger, source.Identifier)
	if source.SourceType != meta.SourceTypeFacebookMessages || !valid {
		broadcastReadError(w, sql.ErrNoRows)
		return
	}
	writeJSON(w, http.StatusOK, ProjectBroadcastDetail(b, rows, src.PageID))
}
func (s *Server) handleBroadcastSubscriptions(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.broadcastReader(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	if q.Get("profile") == "" {
		writeError(w, http.StatusBadRequest, "invalid_args", "profile is required")
		return
	}
	in, err := ParseCLIRunBroadcast(CLIRunRequest{Args: append([]string{"broadcast", "subscriptions", q.Get("profile")}, broadcastSubscriptionQueryArgs(q.Get("participant"), q.Get("status"))...)})
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_args", "Invalid subscription filter")
		return
	}
	source, _, err := s.broadcastSource(r.Context(), reader, in.Profile)
	if err != nil {
		broadcastReadError(w, err)
		return
	}
	rows, err := reader.ListMarketingSubscriptionsContext(r.Context(), source.ID, store.SubscriptionFilter{ParticipantIDs: in.Participants, Status: in.Status})
	if err != nil {
		broadcastReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, BroadcastSubscriptionsResponse{Subscriptions: ProjectBroadcastSubscriptions(rows)})
}
func broadcastSubscriptionQueryArgs(participant, status string) []string {
	var args []string
	if participant != "" {
		if _, err := strconv.ParseInt(participant, 10, 64); err != nil {
			return []string{"--participant=invalid"}
		}
		args = append(args, "--participant", participant)
	}
	if status != "" {
		args = append(args, "--status", status)
	}
	return args
}
