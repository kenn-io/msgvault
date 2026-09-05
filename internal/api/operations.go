package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/store"
)

const (
	operationRunsDefaultLimit     = 25
	maxOperationTokenPayloadBytes = 16 * 1024
	maxOperationArchiveUIDBytes   = 1024
	operationPersonEnrichmentJob  = "person-enrichment"
)

var (
	errInvalidOperationRunReference = errors.New("invalid operation run reference")
	errInvalidOperationCursor       = errors.New("invalid operation history cursor")
)

type OperationPublicCounter struct {
	Name  operations.CounterName `json:"name" enum:"added,attempted,books,created,failed,identity_rejected,item_errors,processed,projected_writes,removed,requested,skipped,started,succeeded,suppressed,truncated,updated"`
	Unit  operations.CounterUnit `json:"unit" enum:"attachments,books,chunks,contacts,documents,messages,people,writes"`
	Value int64                  `json:"value"`
}

type OperationPublicError struct {
	Code    operations.PublicErrorCode `json:"code" enum:"archive_gap,authentication_failed,budget,cancelled,carddav_sync_failed,daemon_restarted,internal,invalid_output,invocation_archive_drift,invocation_authentication_failed,invocation_cancelled,invocation_daemon_restarted,invocation_internal,invocation_invalid_output,invocation_rate_limited,invocation_safety_limit,invocation_timeout,invocation_unsafe_error_redacted,invocation_upstream_failed,lease_lost,person_sweep_failed,policy,provider_http,rate_limited,retry_after,safety_limit,source_sync_failed,sync_failed,timeout,unsafe_error_redacted,upstream_failed"`
	Message string                     `json:"message"`
}

// OperationErrorResponse is the closed public error envelope for Operations
// routes. Keep this separate from the legacy ErrorResponse: unrelated routes
// retain their existing open response contract while Operations clients can
// reject fields that were never approved for publication.
type OperationErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

type OperationRunSummary struct {
	ID         string                   `json:"id"`
	Kind       operations.Kind          `json:"kind" enum:"carddav_sync,document_embedding,document_extraction,message_embedding,person_embedding,person_enrichment,person_sweep,source_sync,visual_embedding"`
	Lane       operations.Lane          `json:"lane" enum:"contacts,documents,messages,person_facts,visual_attachments"`
	State      operations.State         `json:"state" enum:"cancelled,failed,partial,queued,running,succeeded"`
	Trigger    *operations.Trigger      `json:"trigger,omitempty" enum:"manual,scheduled"`
	StartedAt  time.Time                `json:"started_at"`
	FinishedAt *time.Time               `json:"finished_at,omitempty"`
	Counters   []OperationPublicCounter `json:"counters"`
	Error      *OperationPublicError    `json:"error,omitempty"`
}

type OperationRunDetail struct {
	OperationRunSummary

	RelatedStatus    *operations.RelatedStatusID `json:"related_status,omitempty" enum:"listSourceStatus,getDocumentIndexStatus,getDocumentVectorStatus,getVisualAttachmentStatus,getCardDAVStatus"`
	SupportedActions []operations.ActionID       `json:"supported_actions" enum:"carddav_sync,visual_build,visual_resume" nullable:"false"`
}

type OperationUnavailableKind struct {
	Kind            operations.Kind `json:"kind" enum:"carddav_sync,document_embedding,document_extraction,message_embedding,person_embedding,person_enrichment,person_sweep,source_sync,visual_embedding"`
	Lane            operations.Lane `json:"lane" enum:"contacts,documents,messages,person_facts,visual_attachments"`
	UnavailableCode string          `json:"unavailable_code"`
}

type OperationRunsResponse struct {
	Runs               []OperationRunSummary      `json:"runs"`
	NextCursor         string                     `json:"next_cursor,omitempty"`
	MembershipRevision int64                      `json:"membership_revision" minimum:"0"`
	UnavailableKinds   []OperationUnavailableKind `json:"unavailable_kinds"`
}

type OperationLaneStatus struct {
	Kind                operations.Kind                `json:"kind" enum:"carddav_sync,document_embedding,document_extraction,message_embedding,person_embedding,person_enrichment,person_sweep,source_sync,visual_embedding"`
	Lane                operations.Lane                `json:"lane" enum:"contacts,documents,messages,person_facts,visual_attachments"`
	Configured          bool                           `json:"configured"`
	HistoryAvailability operations.HistoryAvailability `json:"history_availability" enum:"available,unavailable"`
	UnavailableCode     string                         `json:"unavailable_code,omitempty"`
	Active              *OperationRunSummary           `json:"active,omitempty"`
	Latest              *OperationRunSummary           `json:"latest,omitempty"`
	LatestSuccessful    *OperationRunSummary           `json:"latest_successful,omitempty"`
	RelatedStatus       *operations.RelatedStatusID    `json:"related_status,omitempty" enum:"listSourceStatus,getDocumentIndexStatus,getDocumentVectorStatus,getVisualAttachmentStatus,getCardDAVStatus"`
	SupportedActions    []operations.ActionID          `json:"supported_actions" enum:"carddav_sync,visual_build,visual_resume"`
}

type OperationStatusResponse struct {
	Lanes []OperationLaneStatus `json:"lanes"`
}

type operationRunReferencePayload struct {
	Kind     operations.Kind         `json:"kind"`
	IDType   operations.StableIDType `json:"id_type"`
	IntID    *int64                  `json:"int_id,omitempty"`
	StringID *string                 `json:"string_id,omitempty"`
}

type operationCursorPayload struct {
	Timestamp          string                  `json:"t"`
	Kind               operations.Kind         `json:"k"`
	IDType             operations.StableIDType `json:"it"`
	IntID              *int64                  `json:"i,omitempty"`
	StringID           *string                 `json:"s,omitempty"`
	FilterHash         string                  `json:"f"`
	MembershipRevision int64                   `json:"r"`
	AvailableKinds     []operations.Kind       `json:"ak"`
	UnavailableKinds   []operations.Kind       `json:"uk"`
}

type operationCursorBinding struct {
	Position           operations.Position
	MembershipRevision int64
	AvailableKinds     []operations.Kind
	UnavailableKinds   []operations.Kind
}

// operationHistoryFilter retains the five HTTP-owned semantic filters. The
// normalized store query expands a lane to kinds, but the cursor must still
// bind the exact public filter so a client cannot silently change its walk.
type operationHistoryFilter struct {
	Kind          operations.Kind
	Lane          operations.Lane
	State         operations.State
	StartedFrom   *time.Time
	StartedBefore *time.Time
}

type operationRunsQuery struct {
	Query  operations.Query
	filter operationHistoryFilter
	cursor *operationCursorBinding
}

func (codec operationTokenCodec) encodeRunReference(
	ctx context.Context, id operations.StableID, archiveUID string,
) (string, error) {
	if err := id.Validate(); err != nil {
		return "", fmt.Errorf("encode operation run reference: %w", err)
	}
	if err := validateOperationArchiveUID(archiveUID); err != nil {
		return "", fmt.Errorf("encode operation run reference: %w", err)
	}
	payload := operationRunReferencePayload{Kind: id.Kind(), IDType: id.Type()}
	switch id.Type() {
	case operations.StableIDInt64:
		value, ok := id.Int64()
		if !ok {
			return "", errors.New("encode operation run reference: numeric ID is unavailable")
		}
		payload.IntID = &value
	case operations.StableIDText:
		value, ok := id.Text()
		if !ok {
			return "", errors.New("encode operation run reference: text ID is unavailable")
		}
		payload.StringID = &value
	default:
		return "", errors.New("encode operation run reference: unsupported ID type")
	}
	return codec.seal(ctx, archiveUID, payload)
}

func (codec operationTokenCodec) decodeRunReference(
	ctx context.Context, raw string, archiveUID string,
) (operations.StableID, error) {
	if err := validateOperationArchiveUID(archiveUID); err != nil {
		return operations.StableID{}, invalidOperationRunReference(err)
	}
	decoded, err := codec.open(ctx, archiveUID, raw)
	if err != nil {
		return operations.StableID{}, invalidOperationRunReference(err)
	}
	var payload operationRunReferencePayload
	fields, err := decodeStrictOperationObject(decoded, &payload, operationRunReferenceFieldAllowed)
	if err != nil {
		return operations.StableID{}, invalidOperationRunReference(err)
	}
	id, err := operationStableID(payload.Kind, payload.IDType,
		payload.IntID, operationFieldPresent(fields, "int_id"),
		payload.StringID, operationFieldPresent(fields, "string_id"))
	if err != nil {
		return operations.StableID{}, invalidOperationRunReference(err)
	}
	return id, nil
}

func (codec operationTokenCodec) encodeCursor(
	ctx context.Context, binding operationCursorBinding, filter operationHistoryFilter, archiveUID string,
) (string, error) {
	if err := binding.validate(filter); err != nil {
		return "", fmt.Errorf("encode operation cursor: %w", err)
	}
	if err := filter.validate(); err != nil {
		return "", fmt.Errorf("encode operation cursor: %w", err)
	}
	if err := validateOperationArchiveUID(archiveUID); err != nil {
		return "", fmt.Errorf("encode operation cursor: %w", err)
	}
	payload := operationCursorPayload{
		Timestamp:          binding.Position.StartedAt.Format(time.RFC3339Nano),
		Kind:               binding.Position.ID.Kind(),
		IDType:             binding.Position.ID.Type(),
		FilterHash:         operationFilterFingerprint(filter),
		MembershipRevision: binding.MembershipRevision,
		AvailableKinds:     append([]operations.Kind{}, binding.AvailableKinds...),
		UnavailableKinds:   append([]operations.Kind{}, binding.UnavailableKinds...),
	}
	switch binding.Position.ID.Type() {
	case operations.StableIDInt64:
		value, _ := binding.Position.ID.Int64()
		payload.IntID = &value
	case operations.StableIDText:
		value, _ := binding.Position.ID.Text()
		payload.StringID = &value
	default:
		return "", errors.New("encode operation cursor: unsupported ID type")
	}
	return codec.seal(ctx, archiveUID, payload)
}

func (codec operationTokenCodec) decodeCursor(
	ctx context.Context, raw string, filter operationHistoryFilter, archiveUID string,
) (operationCursorBinding, error) {
	if err := filter.validate(); err != nil {
		return operationCursorBinding{}, invalidOperationCursor(err)
	}
	if err := validateOperationArchiveUID(archiveUID); err != nil {
		return operationCursorBinding{}, invalidOperationCursor(err)
	}
	decoded, err := codec.open(ctx, archiveUID, raw)
	if err != nil {
		return operationCursorBinding{}, invalidOperationCursor(err)
	}
	var payload operationCursorPayload
	fields, err := decodeStrictOperationObject(decoded, &payload, operationCursorFieldAllowed)
	if err != nil {
		return operationCursorBinding{}, invalidOperationCursor(err)
	}
	for _, required := range []string{"t", "k", "it", "f", "r", "ak", "uk"} {
		if !operationFieldPresent(fields, required) {
			return operationCursorBinding{}, invalidOperationCursor(
				fmt.Errorf("required cursor field %q is missing", required))
		}
	}
	if payload.AvailableKinds == nil || payload.UnavailableKinds == nil {
		return operationCursorBinding{}, invalidOperationCursor(
			errors.New("operation cursor kind sets must be arrays"))
	}
	if !validOperationFilterHash(payload.FilterHash) || payload.FilterHash != operationFilterFingerprint(filter) {
		return operationCursorBinding{}, invalidOperationCursor(errors.New("filter binding does not match"))
	}
	startedAt, err := time.Parse(time.RFC3339Nano, payload.Timestamp)
	if err != nil || startedAt.Location() != time.UTC ||
		startedAt.Format(time.RFC3339Nano) != payload.Timestamp {
		return operationCursorBinding{}, invalidOperationCursor(
			errors.New("timestamp must be canonical RFC3339 UTC"))
	}
	id, err := operationStableID(payload.Kind, payload.IDType,
		payload.IntID, operationFieldPresent(fields, "i"),
		payload.StringID, operationFieldPresent(fields, "s"))
	if err != nil {
		return operationCursorBinding{}, invalidOperationCursor(err)
	}
	binding := operationCursorBinding{
		Position:           operations.Position{StartedAt: startedAt, ID: id},
		MembershipRevision: payload.MembershipRevision,
		AvailableKinds:     slices.Clone(payload.AvailableKinds),
		UnavailableKinds:   slices.Clone(payload.UnavailableKinds),
	}
	if err := binding.validate(filter); err != nil {
		return operationCursorBinding{}, invalidOperationCursor(err)
	}
	return binding, nil
}

func (binding operationCursorBinding) validate(filter operationHistoryFilter) error {
	if err := binding.Position.Validate(); err != nil {
		return err
	}
	if !filter.allows(binding.Position.ID.Kind()) {
		return errors.New("operation cursor position kind is outside the filter")
	}
	if binding.MembershipRevision < 0 {
		return errors.New("operation cursor membership revision is negative")
	}
	if err := validateOperationCursorKinds(binding.AvailableKinds); err != nil {
		return fmt.Errorf("available kinds: %w", err)
	}
	if err := validateOperationCursorKinds(binding.UnavailableKinds); err != nil {
		return fmt.Errorf("unavailable kinds: %w", err)
	}
	if !slices.Contains(binding.AvailableKinds, binding.Position.ID.Kind()) {
		return errors.New("operation cursor position kind is unavailable")
	}
	for _, kind := range binding.AvailableKinds {
		if slices.Contains(binding.UnavailableKinds, kind) {
			return errors.New("operation cursor kind is both available and unavailable")
		}
	}
	return nil
}

func validateOperationCursorKinds(kinds []operations.Kind) error {
	for index, kind := range kinds {
		if err := kind.Validate(); err != nil {
			return err
		}
		if index > 0 && kinds[index-1] >= kind {
			return errors.New("operation cursor kinds must be sorted and unique")
		}
	}
	return nil
}

func parseOperationRunsQuery(
	r *http.Request, codec operationTokenCodec, archiveUID string,
) (operationRunsQuery, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return operationRunsQuery{}, newParamError("query", "operation history query is malformed")
	}
	allowed := map[string]struct{}{
		"kind": {}, "lane": {}, "state": {}, "started_from": {}, "started_before": {},
		"limit": {}, "cursor": {},
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if _, ok := allowed[key]; !ok {
			return operationRunsQuery{}, newParamError("query",
				fmt.Sprintf("unknown operation history query parameter %q", key))
		}
		if len(values[key]) != 1 {
			return operationRunsQuery{}, newParamError(key,
				fmt.Sprintf("query parameter %q must appear exactly once", key))
		}
	}

	filter := operationHistoryFilter{}
	if raw, present := operationSingleQueryValue(values, "kind"); present {
		filter.Kind = operations.Kind(raw)
		if err := filter.Kind.Validate(); err != nil {
			return operationRunsQuery{}, enumParamError("kind", raw, operationKindValues())
		}
	}
	if raw, present := operationSingleQueryValue(values, "lane"); present {
		filter.Lane = operations.Lane(raw)
		if err := filter.Lane.Validate(); err != nil {
			return operationRunsQuery{}, enumParamError("lane", raw, operationLaneValues())
		}
	}
	if raw, present := operationSingleQueryValue(values, "state"); present {
		filter.State = operations.State(raw)
		if err := filter.State.Validate(); err != nil {
			return operationRunsQuery{}, enumParamError("state", raw, operationStateValues())
		}
	}
	if err := filter.validate(); err != nil {
		return operationRunsQuery{}, newParamError("lane", err.Error())
	}
	if raw, present := operationSingleQueryValue(values, "started_from"); present {
		filter.StartedFrom, err = parseCanonicalOperationTime("started_from", raw)
		if err != nil {
			return operationRunsQuery{}, err
		}
	}
	if raw, present := operationSingleQueryValue(values, "started_before"); present {
		filter.StartedBefore, err = parseCanonicalOperationTime("started_before", raw)
		if err != nil {
			return operationRunsQuery{}, err
		}
	}
	if err := filter.validate(); err != nil {
		return operationRunsQuery{}, newParamError("started_before", err.Error())
	}

	limit := operationRunsDefaultLimit
	if raw, present := operationSingleQueryValue(values, "limit"); present {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			return operationRunsQuery{}, newParamError("limit",
				"query parameter \"limit\" must be an integer between 1 and 100")
		}
	}

	query := operations.Query{
		Kinds:         operationFilterKinds(filter),
		States:        operationFilterStates(filter),
		StartedFrom:   cloneOperationTime(filter.StartedFrom),
		StartedBefore: cloneOperationTime(filter.StartedBefore),
		Limit:         limit,
	}
	var cursor *operationCursorBinding
	if raw, present := operationSingleQueryValue(values, "cursor"); present {
		if raw == "" {
			return operationRunsQuery{}, invalidOperationCursor(errors.New("cursor is empty"))
		}
		binding, decodeErr := codec.decodeCursor(r.Context(), raw, filter, archiveUID)
		if decodeErr != nil {
			return operationRunsQuery{}, decodeErr
		}
		cursor = &binding
		query.Position = &binding.Position
	}
	if err := query.Validate(); err != nil {
		if query.Position != nil {
			return operationRunsQuery{}, invalidOperationCursor(err)
		}
		return operationRunsQuery{}, newParamError("query", err.Error())
	}
	return operationRunsQuery{Query: query, filter: filter, cursor: cursor}, nil
}

func parseCanonicalOperationTime(name, raw string) (*time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil || parsed.Location() != time.UTC || parsed.Format(time.RFC3339Nano) != raw {
		return nil, newParamError(name,
			fmt.Sprintf("query parameter %q must be canonical UTC RFC 3339", name))
	}
	return &parsed, nil
}

func cloneOperationTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func operationSingleQueryValue(values url.Values, name string) (string, bool) {
	raw, present := values[name]
	if !present {
		return "", false
	}
	return raw[0], true
}

func (filter operationHistoryFilter) validate() error {
	if filter.Kind != "" {
		if err := filter.Kind.Validate(); err != nil {
			return err
		}
	}
	if filter.Lane != "" {
		if err := filter.Lane.Validate(); err != nil {
			return err
		}
	}
	if filter.State != "" {
		if err := filter.State.Validate(); err != nil {
			return err
		}
	}
	if filter.Kind != "" && filter.Lane != "" && !operationKindUsesLane(filter.Kind, filter.Lane) {
		return fmt.Errorf("operation kind %q does not belong to lane %q", filter.Kind, filter.Lane)
	}
	if filter.StartedFrom != nil && (filter.StartedFrom.IsZero() || filter.StartedFrom.Location() != time.UTC) {
		return errors.New("operation history lower date bound must be normalized to UTC")
	}
	if filter.StartedBefore != nil && (filter.StartedBefore.IsZero() || filter.StartedBefore.Location() != time.UTC) {
		return errors.New("operation history upper date bound must be normalized to UTC")
	}
	if filter.StartedFrom != nil && filter.StartedBefore != nil && !filter.StartedFrom.Before(*filter.StartedBefore) {
		return errors.New("operation history lower date bound must precede upper date bound")
	}
	return nil
}

func (filter operationHistoryFilter) allows(kind operations.Kind) bool {
	if filter.Kind != "" && filter.Kind != kind {
		return false
	}
	return filter.Lane == "" || operationKindUsesLane(kind, filter.Lane)
}

func operationFilterFingerprint(filter operationHistoryFilter) string {
	canonical := fmt.Sprintf("kind=%s\nlane=%s\nstate=%s\nstarted_from=%s\nstarted_before=%s\n",
		filter.Kind, filter.Lane, filter.State,
		formatOperationFilterTime(filter.StartedFrom), formatOperationFilterTime(filter.StartedBefore))
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}

func formatOperationFilterTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.Format(time.RFC3339Nano)
}

func operationFilterKinds(filter operationHistoryFilter) []operations.Kind {
	if filter.Kind != "" {
		return []operations.Kind{filter.Kind}
	}
	if filter.Lane == "" {
		return nil
	}
	kinds := make([]operations.Kind, 0)
	for _, definition := range operations.LaneRegistry() {
		if definition.Lane == filter.Lane {
			kinds = append(kinds, definition.Kind)
		}
	}
	slices.Sort(kinds)
	return kinds
}

func operationFilterStates(filter operationHistoryFilter) []operations.State {
	if filter.State == "" {
		return nil
	}
	return []operations.State{filter.State}
}

func operationKindUsesLane(kind operations.Kind, lane operations.Lane) bool {
	for _, definition := range operations.LaneRegistry() {
		if definition.Kind == kind {
			return definition.Lane == lane
		}
	}
	return false
}

func operationKindValues() []string {
	definitions := operations.LaneRegistry()
	values := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		values = append(values, string(definition.Kind))
	}
	return values
}

func operationLaneValues() []string {
	values := []string{
		string(operations.LaneMessages),
		string(operations.LanePersonFacts),
		string(operations.LaneContacts),
		string(operations.LaneDocuments),
		string(operations.LaneVisualAttachments),
	}
	slices.Sort(values)
	return values
}

func operationStateValues() []string {
	values := []string{
		string(operations.StateQueued),
		string(operations.StateRunning),
		string(operations.StateSucceeded),
		string(operations.StatePartial),
		string(operations.StateFailed),
		string(operations.StateCancelled),
	}
	slices.Sort(values)
	return values
}

func (s *Server) handleOperationRuns(w http.ResponseWriter, r *http.Request) {
	if s.operationHistoryReader == nil {
		writeOperationHistoryUnavailable(w)
		return
	}
	archiveUID, ok := s.operationHistoryArchiveUID(w, r)
	if !ok {
		return
	}
	codec, ok := s.operationHistoryTokenCodec(w)
	if !ok {
		return
	}
	parsed, err := parseOperationRunsQuery(r, codec, archiveUID)
	if err != nil {
		if errors.Is(err, errInvalidOperationCursor) {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "Operation history cursor is invalid")
			return
		}
		s.rejectBadParam(w, err)
		return
	}
	snapshot, err := s.operationHistoryReader.ListRuns(r.Context(), parsed.Query)
	if err != nil {
		s.writeOperationHistoryReaderError(w, err)
		return
	}
	if err := validateOperationHistorySnapshot(snapshot, parsed.Query, parsed.filter); err != nil {
		writeError(w, http.StatusInternalServerError, "operation_history_failed",
			"Operation history could not be read")
		return
	}
	if parsed.cursor != nil && !operationCursorMatchesSnapshot(*parsed.cursor, snapshot) {
		writeError(w, http.StatusConflict, "operation_history_conflict",
			"Operation history changed while it was being read")
		return
	}
	if parsed.filter.Kind != "" && slices.Contains(snapshot.UnavailableKinds, parsed.filter.Kind) {
		writeOperationHistoryUnavailable(w)
		return
	}
	runs := snapshot.Runs
	if snapshot.Position != nil {
		runs = runs[:parsed.Query.Limit]
	}
	response := OperationRunsResponse{
		Runs:               make([]OperationRunSummary, 0, len(runs)),
		MembershipRevision: snapshot.MembershipRevision,
		UnavailableKinds:   unavailableOperationKinds(snapshot.UnavailableKinds),
	}
	for _, run := range runs {
		summary, projectErr := operationRunSummary(r.Context(), codec, run, archiveUID)
		if projectErr != nil {
			writeError(w, http.StatusInternalServerError, "operation_history_failed",
				"Operation history could not be read")
			return
		}
		response.Runs = append(response.Runs, summary)
	}
	if snapshot.Position != nil {
		response.NextCursor, err = codec.encodeCursor(r.Context(), operationCursorBinding{
			Position:           *snapshot.Position,
			MembershipRevision: snapshot.MembershipRevision,
			AvailableKinds:     snapshot.AvailableKinds,
			UnavailableKinds:   snapshot.UnavailableKinds,
		}, parsed.filter, archiveUID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "operation_history_failed",
				"Operation history could not be read")
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func validateOperationHistorySnapshot(
	snapshot operations.HistorySnapshot, query operations.Query, filter operationHistoryFilter,
) error {
	if snapshot.MembershipRevision < 0 {
		return errors.New("operation history membership revision is negative")
	}
	if err := validateOperationCursorKinds(snapshot.AvailableKinds); err != nil {
		return fmt.Errorf("validate available operation history kinds: %w", err)
	}
	if err := validateOperationCursorKinds(snapshot.UnavailableKinds); err != nil {
		return fmt.Errorf("validate unavailable operation history kinds: %w", err)
	}
	for _, kind := range snapshot.AvailableKinds {
		if slices.Contains(snapshot.UnavailableKinds, kind) || !filter.allows(kind) {
			return errors.New("operation history availability set is inconsistent")
		}
	}
	for _, kind := range snapshot.UnavailableKinds {
		if !filter.allows(kind) {
			return errors.New("operation history unavailable set is outside the filter")
		}
	}
	if len(snapshot.Runs) > query.Limit+1 {
		return errors.New("operation history returned too many runs")
	}
	for _, run := range snapshot.Runs {
		if err := run.Validate(); err != nil || !slices.Contains(snapshot.AvailableKinds, run.ID.Kind()) {
			return errors.New("operation history returned an invalid or unavailable run")
		}
	}
	if snapshot.Position == nil {
		if len(snapshot.Runs) > query.Limit {
			return errors.New("operation history continuation position is missing")
		}
		return nil
	}
	if len(snapshot.Runs) != query.Limit+1 {
		return errors.New("operation history continuation was not proven")
	}
	want := operations.Position{
		StartedAt: snapshot.Runs[query.Limit-1].StartedAt,
		ID:        snapshot.Runs[query.Limit-1].ID,
	}
	if *snapshot.Position != want {
		return errors.New("operation history continuation position is inconsistent")
	}
	return nil
}

func operationCursorMatchesSnapshot(cursor operationCursorBinding, snapshot operations.HistorySnapshot) bool {
	return cursor.MembershipRevision == snapshot.MembershipRevision &&
		slices.Equal(cursor.AvailableKinds, snapshot.AvailableKinds) &&
		slices.Equal(cursor.UnavailableKinds, snapshot.UnavailableKinds)
}

func (s *Server) handleOperationStatus(w http.ResponseWriter, r *http.Request) {
	visualConfigured, visualActions := s.operationVisualAdvertisement(r.Context())
	response := OperationStatusResponse{
		Lanes: make([]OperationLaneStatus, 0, len(operations.LaneRegistry())),
	}
	for _, definition := range operations.LaneRegistry() {
		lane := OperationLaneStatus{
			Kind: definition.Kind, Lane: definition.Lane,
			Configured:          s.operationLaneConfigured(r.Context(), definition.Kind, visualConfigured),
			HistoryAvailability: operations.HistoryAvailable,
			RelatedStatus:       operationRelatedStatus(definition.Kind),
		}
		s.projectOperationLaneHistory(r.Context(), &lane)
		lane.SupportedActions = operationSupportedActions(
			definition.Kind, lane.Configured, lane.HistoryAvailability, lane.Active != nil, visualActions)
		response.Lanes = append(response.Lanes, lane)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) projectOperationLaneHistory(ctx context.Context, lane *OperationLaneStatus) {
	if s.operationHistoryReader == nil {
		degradeOperationLaneHistory(lane)
		return
	}
	status, err := s.operationHistoryReader.LaneStatus(ctx, lane.Kind)
	if err != nil || status.Validate() != nil {
		degradeOperationLaneHistory(lane)
		return
	}
	lane.HistoryAvailability = status.HistoryAvailability
	lane.UnavailableCode = status.UnavailableCode
	if status.Active == nil && status.Latest == nil && status.LatestSuccessful == nil {
		return
	}
	identifier, ok := s.store.(ArchiveIdentifier)
	if !ok {
		degradeOperationLaneHistory(lane)
		return
	}
	archiveUID, err := identifier.ArchiveUIDContext(ctx)
	if err != nil || validateOperationArchiveUID(archiveUID) != nil {
		degradeOperationLaneHistory(lane)
		return
	}
	keyring, ok := s.store.(operationTokenKeyring)
	if !ok {
		degradeOperationLaneHistory(lane)
		return
	}
	codec := newOperationTokenCodec(keyring)
	lane.Active, err = operationRunSummaryPointer(ctx, codec, status.Active, archiveUID)
	if err == nil {
		lane.Latest, err = operationRunSummaryPointer(ctx, codec, status.Latest, archiveUID)
		if err == nil && status.Active != nil && status.Latest != nil && status.Active.ID == status.Latest.ID {
			lane.Latest.ID = lane.Active.ID
		}
	}
	if err == nil {
		lane.LatestSuccessful, err = operationRunSummaryPointer(ctx, codec, status.LatestSuccessful, archiveUID)
		if err == nil && status.LatestSuccessful != nil {
			switch {
			case status.Active != nil && status.LatestSuccessful.ID == status.Active.ID:
				lane.LatestSuccessful.ID = lane.Active.ID
			case status.Latest != nil && status.LatestSuccessful.ID == status.Latest.ID:
				lane.LatestSuccessful.ID = lane.Latest.ID
			}
		}
	}
	if err != nil {
		degradeOperationLaneHistory(lane)
	}
}

func operationRunSummaryPointer(
	ctx context.Context, codec operationTokenCodec, run *operations.Run, archiveUID string,
) (*OperationRunSummary, error) {
	if run == nil {
		return nil, nil //nolint:nilnil // An absent lane run is a valid optional projection.
	}
	summary, err := operationRunSummary(ctx, codec, *run, archiveUID)
	if err != nil {
		return nil, err
	}
	return &summary, nil
}

func degradeOperationLaneHistory(lane *OperationLaneStatus) {
	lane.HistoryAvailability = operations.HistoryUnavailable
	lane.UnavailableCode = string(lane.Kind) + "_history_unavailable"
	lane.Active = nil
	lane.Latest = nil
	lane.LatestSuccessful = nil
}

type operationSourceLister interface {
	ListSourcesContext(ctx context.Context, account string) ([]*store.Source, error)
}

func (s *Server) operationLaneConfigured(
	ctx context.Context, kind operations.Kind, visualConfigured bool,
) bool {
	if s.cfg == nil {
		return false
	}
	switch kind {
	case operations.KindCardDAVSync:
		if s.cardDAV == nil {
			return false
		}
		status, err := s.cardDAV.Status(ctx)
		return err == nil && status.Configured && status.Available && status.CredentialConfigured
	case operations.KindDocumentEmbedding:
		return s.cfg.Vector.Enabled && s.cfg.Attachments.Documents.Index.Embeddings.Enabled
	case operations.KindDocumentExtraction:
		if !s.cfg.Attachments.Documents.Enabled {
			return false
		}
		_, err := s.currentDocumentIndexStatusRequest(ctx)
		return !errors.Is(err, store.ErrDocumentIndexStatusScopeUnavailable)
	case operations.KindMessageEmbedding:
		return s.cfg.Vector.Enabled
	case operations.KindPersonEmbedding:
		return s.cfg.Vector.Enabled && s.cfg.Vector.People.Enabled
	case operations.KindPersonEnrichment:
		if !s.cfg.People.Enrichment.Enabled || s.scheduler == nil ||
			!s.scheduler.IsJobScheduled(operationPersonEnrichmentJob) {
			return false
		}
		return slices.ContainsFunc(s.cfg.People.Enrichment.Providers, func(provider personenrichment.ProviderConfig) bool {
			return provider.Enabled
		})
	case operations.KindPersonSweep:
		return s.cfg.People.Sweep.Enabled
	case operations.KindSourceSync:
		sources, ok := s.store.(operationSourceLister)
		if !ok {
			return false
		}
		rows, err := sources.ListSourcesContext(ctx, "")
		return err == nil && len(rows) > 0
	case operations.KindVisualEmbedding:
		return visualConfigured
	default:
		return false
	}
}

func (s *Server) operationVisualAdvertisement(ctx context.Context) (bool, []operations.ActionID) {
	s.vectorMu.RLock()
	build, run, statusFn := s.visualBuild, s.visualRun, s.visualStatus
	s.vectorMu.RUnlock()
	if statusFn == nil {
		return false, []operations.ActionID{}
	}
	status, err := statusFn(ctx, false)
	if err != nil {
		return true, []operations.ActionID{}
	}
	switch status.Generation.State {
	case store.VisualGenerationBuilding:
		if status.Generation.Consented && run != nil {
			return true, []operations.ActionID{operations.ActionVisualResume}
		}
		if !status.Generation.Consented && build != nil {
			return true, []operations.ActionID{operations.ActionVisualBuild}
		}
	case store.VisualGenerationActive:
		complete := status.ReconciliationComplete && status.JournalLag == 0 && status.Stale == 0 &&
			status.Converged == status.ConvergenceTotal
		if !complete && run != nil {
			return true, []operations.ActionID{operations.ActionVisualResume}
		}
	case store.VisualGenerationRetired:
		// Retired generations advertise no action until a new build is configured.
	}
	return true, []operations.ActionID{}
}

func operationRelatedStatus(kind operations.Kind) *operations.RelatedStatusID {
	var related operations.RelatedStatusID
	switch kind {
	case operations.KindCardDAVSync:
		related = operations.RelatedStatusCardDAV
	case operations.KindDocumentEmbedding:
		related = operations.RelatedStatusDocumentVector
	case operations.KindDocumentExtraction:
		related = operations.RelatedStatusDocumentIndex
	case operations.KindSourceSync:
		related = operations.RelatedStatusSource
	case operations.KindVisualEmbedding:
		related = operations.RelatedStatusVisual
	default:
		return nil
	}
	return &related
}

func operationSupportedActions(
	kind operations.Kind,
	configured bool,
	historyAvailability operations.HistoryAvailability,
	hasActive bool,
	visualActions []operations.ActionID,
) []operations.ActionID {
	actions := make([]operations.ActionID, 0)
	switch kind {
	case operations.KindCardDAVSync:
		if configured && historyAvailability == operations.HistoryAvailable && !hasActive {
			actions = append(actions, operations.ActionCardDAVSync)
		}
	case operations.KindVisualEmbedding:
		actions = append(actions, visualActions...)
	default:
		// The closed operation registry exposes no direct action for this kind.
	}
	return actions
}

func (s *Server) handleOperationRunDetail(w http.ResponseWriter, r *http.Request) {
	if s.operationHistoryReader == nil {
		writeOperationHistoryUnavailable(w)
		return
	}
	archiveUID, ok := s.operationHistoryArchiveUID(w, r)
	if !ok {
		return
	}
	codec, ok := s.operationHistoryTokenCodec(w)
	if !ok {
		return
	}
	id, err := codec.decodeRunReference(r.Context(), r.PathValue("id"), archiveUID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_operation_run_id",
			"Operation run ID is invalid")
		return
	}
	run, err := s.operationHistoryReader.GetRun(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrOperationRunNotFound):
			writeError(w, http.StatusNotFound, "operation_run_not_found", "Operation run was not found")
		case errors.Is(err, store.ErrOperationHistoryUnavailable):
			writeOperationHistoryUnavailable(w)
		default:
			s.writeOperationHistoryReaderError(w, err)
		}
		return
	}
	summary, err := operationRunSummary(r.Context(), codec, run, archiveUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "operation_history_failed",
			"Operation history could not be read")
		return
	}
	summary.ID = r.PathValue("id")
	detail := OperationRunDetail{
		OperationRunSummary: summary,
		RelatedStatus:       operationRelatedStatus(run.ID.Kind()),
	}
	configured := false
	historyAvailability := operations.HistoryUnavailable
	hasActive := false
	visualActions := make([]operations.ActionID, 0)
	switch run.ID.Kind() {
	case operations.KindCardDAVSync:
		status, statusErr := s.operationHistoryReader.LaneStatus(r.Context(), run.ID.Kind())
		if statusErr == nil && status.Validate() == nil {
			historyAvailability = status.HistoryAvailability
			hasActive = status.Active != nil
		}
		configured = s.operationLaneConfigured(r.Context(), run.ID.Kind(), false)
	case operations.KindVisualEmbedding:
		configured, visualActions = s.operationVisualAdvertisement(r.Context())
	default:
		// The remaining kinds have no direct action inputs.
	}
	detail.SupportedActions = operationSupportedActions(
		run.ID.Kind(), configured, historyAvailability, hasActive, visualActions)
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) operationHistoryTokenCodec(w http.ResponseWriter) (operationTokenCodec, bool) {
	keyring, ok := s.store.(operationTokenKeyring)
	if !ok {
		writeOperationHistoryUnavailable(w)
		return operationTokenCodec{}, false
	}
	return newOperationTokenCodec(keyring), true
}

func (s *Server) operationHistoryArchiveUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	identifier, ok := s.store.(ArchiveIdentifier)
	if !ok {
		writeOperationHistoryUnavailable(w)
		return "", false
	}
	archiveUID, err := identifier.ArchiveUIDContext(r.Context())
	if err != nil || validateOperationArchiveUID(archiveUID) != nil {
		writeOperationHistoryUnavailable(w)
		return "", false
	}
	return archiveUID, true
}

func (s *Server) writeOperationHistoryReaderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrOperationHistoryUnavailable):
		writeOperationHistoryUnavailable(w)
	default:
		writeError(w, http.StatusInternalServerError, "operation_history_failed",
			"Operation history could not be read")
	}
}

func writeOperationHistoryUnavailable(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable, "operation_history_unavailable",
		"Operation history is unavailable")
}

func operationRunSummary(
	ctx context.Context, codec operationTokenCodec, run operations.Run, archiveUID string,
) (OperationRunSummary, error) {
	if err := run.Validate(); err != nil {
		return OperationRunSummary{}, fmt.Errorf("project operation run: %w", err)
	}
	ref, err := codec.encodeRunReference(ctx, run.ID, archiveUID)
	if err != nil {
		return OperationRunSummary{}, err
	}
	summary := OperationRunSummary{
		ID: ref, Kind: run.ID.Kind(), Lane: run.Lane, State: run.State,
		Trigger: run.Trigger, StartedAt: run.StartedAt, FinishedAt: run.FinishedAt,
		Counters: make([]OperationPublicCounter, 0, len(run.Counters)),
	}
	for _, counter := range run.Counters {
		summary.Counters = append(summary.Counters, OperationPublicCounter{
			Name: counter.Name, Unit: counter.Unit, Value: counter.Value,
		})
	}
	if run.Error != nil {
		summary.Error = &OperationPublicError{Code: run.Error.Code, Message: run.Error.Message}
	}
	return summary, nil
}

func unavailableOperationKinds(kinds []operations.Kind) []OperationUnavailableKind {
	result := make([]OperationUnavailableKind, 0, len(kinds))
	for _, kind := range kinds {
		for _, definition := range operations.LaneRegistry() {
			if definition.Kind != kind {
				continue
			}
			result = append(result, OperationUnavailableKind{
				Kind: kind, Lane: definition.Lane,
				UnavailableCode: string(kind) + "_history_unavailable",
			})
			break
		}
	}
	return result
}

func operationStableID(
	kind operations.Kind, idType operations.StableIDType,
	intID *int64, intPresent bool, stringID *string, stringPresent bool,
) (operations.StableID, error) {
	if err := kind.Validate(); err != nil {
		return operations.StableID{}, err
	}
	if err := idType.Validate(); err != nil {
		return operations.StableID{}, err
	}
	if intPresent == stringPresent {
		return operations.StableID{}, errors.New("operation token must carry exactly one stable ID")
	}
	switch idType {
	case operations.StableIDInt64:
		if !intPresent || intID == nil || stringPresent {
			return operations.StableID{}, errors.New("operation numeric token has the wrong ID field")
		}
		return operations.NewInt64ID(kind, *intID)
	case operations.StableIDText:
		if !stringPresent || stringID == nil || intPresent {
			return operations.StableID{}, errors.New("operation text token has the wrong ID field")
		}
		return operations.NewTextID(kind, *stringID)
	default:
		return operations.StableID{}, errors.New("operation token has an unsupported ID type")
	}
}

// decodeStrictOperationObject rejects duplicate and unknown fields and
// requires exactly one top-level JSON object. json.Decoder alone permits
// duplicate object names, so the first pass checks names before the typed pass.
func decodeStrictOperationObject(
	encoded []byte, target any, allowed func(string) bool,
) (map[string]struct{}, error) {
	if !utf8.Valid(encoded) {
		return nil, errors.New("operation token payload must be valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' {
		return nil, errors.New("operation token payload must be an object")
	}
	seen := make(map[string]struct{})
	for decoder.More() {
		nameToken, tokenErr := decoder.Token()
		if tokenErr != nil {
			return nil, tokenErr
		}
		name, ok := nameToken.(string)
		if !ok {
			return nil, errors.New("operation token object key is invalid")
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("operation token field %q is duplicated", name)
		}
		if !allowed(name) {
			return nil, fmt.Errorf("operation token field %q is not allowed", name)
		}
		seen[name] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if err := requireOperationJSONEOF(decoder); err != nil {
		return nil, err
	}

	typed := json.NewDecoder(bytes.NewReader(encoded))
	typed.DisallowUnknownFields()
	if err := typed.Decode(target); err != nil {
		return nil, err
	}
	if err := requireOperationJSONEOF(typed); err != nil {
		return nil, err
	}
	return seen, nil
}

func operationRunReferenceFieldAllowed(name string) bool {
	switch name {
	case "kind", "id_type", "int_id", "string_id":
		return true
	default:
		return false
	}
}

func operationCursorFieldAllowed(name string) bool {
	switch name {
	case "t", "k", "it", "i", "s", "f", "r", "ak", "uk":
		return true
	default:
		return false
	}
}

func operationFieldPresent(fields map[string]struct{}, name string) bool {
	_, present := fields[name]
	return present
}

func requireOperationJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("operation token payload has trailing JSON")
		}
		return err
	}
	return nil
}

func validateOperationArchiveUID(value string) error {
	if value == "" || strings.TrimSpace(value) != value || !utf8.ValidString(value) ||
		len(value) > maxOperationArchiveUIDBytes {
		return errors.New("operation cursor archive UID must be nonempty, canonical, and bounded")
	}
	return nil
}

func validOperationFilterHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func invalidOperationRunReference(cause error) error {
	return fmt.Errorf("%w: %w", errInvalidOperationRunReference, cause)
}

func invalidOperationCursor(cause error) error {
	return errors.Join(errInvalidOperationCursor,
		newParamError("cursor", fmt.Sprintf("operation history cursor is invalid: %v", cause)))
}
