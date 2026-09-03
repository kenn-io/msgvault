package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	"go.kenn.io/msgvault/internal/store"
)

const (
	operationTokenVersion         = "1"
	operationRunsDefaultLimit     = 25
	maxOperationTokenPayloadBytes = 16 * 1024
	maxOperationArchiveUIDBytes   = 1024
)

var (
	errInvalidOperationRunReference = errors.New("invalid operation run reference")
	errInvalidOperationCursor       = errors.New("invalid operation history cursor")
)

type OperationPublicCounter struct {
	Name  operations.CounterName `json:"name" enum:"processed,added,updated,item_errors,attempted,succeeded,failed,projected_writes,books,created,removed"`
	Unit  operations.CounterUnit `json:"unit" enum:"messages,people,writes,books,contacts"`
	Value int64                  `json:"value"`
}

type OperationPublicError struct {
	Code    operations.PublicErrorCode `json:"code" enum:"source_sync_failed,person_sweep_failed,policy,budget,lease_lost,rate_limited,timeout,provider_http,invalid_output,archive_gap,internal,cancelled,retry_after,authentication_failed,upstream_failed,safety_limit,sync_failed,unsafe_error_redacted,daemon_restarted,carddav_sync_failed"`
	Message string                     `json:"message"`
}

type OperationRunSummary struct {
	ID         string                   `json:"id"`
	Kind       operations.Kind          `json:"kind" enum:"carddav_sync,document_embedding,document_extraction,message_embedding,person_embedding,person_enrichment,person_sweep,source_sync,visual_embedding"`
	Lane       operations.Lane          `json:"lane" enum:"contacts,documents,messages,person_facts,visual_attachments"`
	State      operations.State         `json:"state" enum:"cancelled,failed,partial,queued,running,succeeded"`
	Trigger    *operations.Trigger      `json:"trigger,omitempty" enum:"manual,scheduled"`
	StartedAt  time.Time                `json:"started_at"`
	FinishedAt *time.Time               `json:"finished_at,omitempty"`
	Counters   []OperationPublicCounter `json:"counters" nullable:"false"`
	Error      *OperationPublicError    `json:"error,omitempty"`
}

type OperationRunDetail struct {
	OperationRunSummary
}

type OperationUnavailableKind struct {
	Kind            operations.Kind `json:"kind" enum:"carddav_sync,document_embedding,document_extraction,message_embedding,person_embedding,person_enrichment,person_sweep,source_sync,visual_embedding"`
	Lane            operations.Lane `json:"lane" enum:"contacts,documents,messages,person_facts,visual_attachments"`
	UnavailableCode string          `json:"unavailable_code"`
}

type OperationRunsResponse struct {
	Runs             []OperationRunSummary      `json:"runs" nullable:"false"`
	NextCursor       string                     `json:"next_cursor,omitempty"`
	UnavailableKinds []OperationUnavailableKind `json:"unavailable_kinds" nullable:"false"`
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
	SupportedActions    []operations.ActionID          `json:"supported_actions" enum:"carddav_sync,visual_build,visual_resume" nullable:"false"`
}

type OperationStatusResponse struct {
	Lanes []OperationLaneStatus `json:"lanes" nullable:"false"`
}

type operationRunReferencePayload struct {
	Kind       operations.Kind         `json:"kind"`
	IDType     operations.StableIDType `json:"id_type"`
	IntID      *int64                  `json:"int_id,omitempty"`
	StringID   *string                 `json:"string_id,omitempty"`
	ArchiveUID string                  `json:"archive_uid"`
}

type operationCursorPayload struct {
	Timestamp  string                  `json:"t"`
	Kind       operations.Kind         `json:"k"`
	IDType     operations.StableIDType `json:"it"`
	IntID      *int64                  `json:"i,omitempty"`
	StringID   *string                 `json:"s,omitempty"`
	FilterHash string                  `json:"f"`
	ArchiveUID string                  `json:"a"`
}

// operationHistoryFilter retains the three HTTP-owned semantic filters. The
// normalized store query expands a lane to kinds, but the cursor must still
// bind the exact public filter so a client cannot silently change its walk.
type operationHistoryFilter struct {
	Kind  operations.Kind
	Lane  operations.Lane
	State operations.State
}

type operationRunsQuery struct {
	Query  operations.Query
	filter operationHistoryFilter
}

func encodeOperationRunReference(id operations.StableID, archiveUID string) (string, error) {
	if err := id.Validate(); err != nil {
		return "", fmt.Errorf("encode operation run reference: %w", err)
	}
	if err := validateOperationArchiveUID(archiveUID); err != nil {
		return "", fmt.Errorf("encode operation run reference: %w", err)
	}
	payload := operationRunReferencePayload{
		Kind: id.Kind(), IDType: id.Type(), ArchiveUID: archiveUID,
	}
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
	return encodeOperationToken(payload)
}

func decodeOperationRunReference(raw string, archiveUID string) (operations.StableID, error) {
	if err := validateOperationArchiveUID(archiveUID); err != nil {
		return operations.StableID{}, invalidOperationRunReference(err)
	}
	decoded, err := decodeOperationToken(raw)
	if err != nil {
		return operations.StableID{}, invalidOperationRunReference(err)
	}
	var payload operationRunReferencePayload
	fields, err := decodeStrictOperationObject(decoded, &payload, operationRunReferenceFieldAllowed)
	if err != nil {
		return operations.StableID{}, invalidOperationRunReference(err)
	}
	if err := validateOperationArchiveUID(payload.ArchiveUID); err != nil {
		return operations.StableID{}, invalidOperationRunReference(err)
	}
	if payload.ArchiveUID != archiveUID {
		return operations.StableID{}, invalidOperationRunReference(errors.New("archive binding does not match"))
	}
	id, err := operationStableID(payload.Kind, payload.IDType,
		payload.IntID, operationFieldPresent(fields, "int_id"),
		payload.StringID, operationFieldPresent(fields, "string_id"))
	if err != nil {
		return operations.StableID{}, invalidOperationRunReference(err)
	}
	return id, nil
}

func encodeOperationCursor(
	position operations.Position, filter operationHistoryFilter, archiveUID string,
) (string, error) {
	if err := position.Validate(); err != nil {
		return "", fmt.Errorf("encode operation cursor: %w", err)
	}
	if err := filter.validate(); err != nil {
		return "", fmt.Errorf("encode operation cursor: %w", err)
	}
	if !filter.allows(position.ID.Kind()) {
		return "", errors.New("encode operation cursor: position kind is outside the filter")
	}
	if err := validateOperationArchiveUID(archiveUID); err != nil {
		return "", fmt.Errorf("encode operation cursor: %w", err)
	}
	payload := operationCursorPayload{
		Timestamp:  position.StartedAt.Format(time.RFC3339Nano),
		Kind:       position.ID.Kind(),
		IDType:     position.ID.Type(),
		FilterHash: operationFilterFingerprint(filter),
		ArchiveUID: archiveUID,
	}
	switch position.ID.Type() {
	case operations.StableIDInt64:
		value, _ := position.ID.Int64()
		payload.IntID = &value
	case operations.StableIDText:
		value, _ := position.ID.Text()
		payload.StringID = &value
	default:
		return "", errors.New("encode operation cursor: unsupported ID type")
	}
	return encodeOperationToken(payload)
}

func decodeOperationCursor(
	raw string, filter operationHistoryFilter, archiveUID string,
) (operations.Position, error) {
	if err := filter.validate(); err != nil {
		return operations.Position{}, invalidOperationCursor(err)
	}
	if err := validateOperationArchiveUID(archiveUID); err != nil {
		return operations.Position{}, invalidOperationCursor(err)
	}
	decoded, err := decodeOperationToken(raw)
	if err != nil {
		return operations.Position{}, invalidOperationCursor(err)
	}
	var payload operationCursorPayload
	fields, err := decodeStrictOperationObject(decoded, &payload, operationCursorFieldAllowed)
	if err != nil {
		return operations.Position{}, invalidOperationCursor(err)
	}
	if payload.ArchiveUID != archiveUID || !validOperationFilterHash(payload.FilterHash) ||
		payload.FilterHash != operationFilterFingerprint(filter) {
		return operations.Position{}, invalidOperationCursor(errors.New("archive or filter binding does not match"))
	}
	if err := validateOperationArchiveUID(payload.ArchiveUID); err != nil {
		return operations.Position{}, invalidOperationCursor(err)
	}
	startedAt, err := time.Parse(time.RFC3339Nano, payload.Timestamp)
	if err != nil || startedAt.Location() != time.UTC {
		return operations.Position{}, invalidOperationCursor(errors.New("timestamp must be RFC3339 UTC"))
	}
	id, err := operationStableID(payload.Kind, payload.IDType,
		payload.IntID, operationFieldPresent(fields, "i"),
		payload.StringID, operationFieldPresent(fields, "s"))
	if err != nil {
		return operations.Position{}, invalidOperationCursor(err)
	}
	if !filter.allows(id.Kind()) {
		return operations.Position{}, invalidOperationCursor(errors.New("position kind is outside the filter"))
	}
	position := operations.Position{StartedAt: startedAt, ID: id}
	if err := position.Validate(); err != nil {
		return operations.Position{}, invalidOperationCursor(err)
	}
	return position, nil
}

func parseOperationRunsQuery(r *http.Request, archiveUID string) (operationRunsQuery, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return operationRunsQuery{}, newParamError("query", "operation history query is malformed")
	}
	allowed := map[string]struct{}{
		"kind": {}, "lane": {}, "state": {}, "limit": {}, "cursor": {},
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

	limit := operationRunsDefaultLimit
	if raw, present := operationSingleQueryValue(values, "limit"); present {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			return operationRunsQuery{}, newParamError("limit",
				"query parameter \"limit\" must be an integer between 1 and 100")
		}
	}

	query := operations.Query{
		Kinds:  operationFilterKinds(filter),
		States: operationFilterStates(filter),
		Limit:  limit,
	}
	if raw, present := operationSingleQueryValue(values, "cursor"); present {
		if raw == "" {
			return operationRunsQuery{}, invalidOperationCursor(errors.New("cursor is empty"))
		}
		position, decodeErr := decodeOperationCursor(raw, filter, archiveUID)
		if decodeErr != nil {
			return operationRunsQuery{}, decodeErr
		}
		query.Position = &position
	}
	if err := query.Validate(); err != nil {
		if query.Position != nil {
			return operationRunsQuery{}, invalidOperationCursor(err)
		}
		return operationRunsQuery{}, newParamError("query", err.Error())
	}
	return operationRunsQuery{Query: query, filter: filter}, nil
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
	return nil
}

func (filter operationHistoryFilter) allows(kind operations.Kind) bool {
	if filter.Kind != "" && filter.Kind != kind {
		return false
	}
	return filter.Lane == "" || operationKindUsesLane(kind, filter.Lane)
}

func operationFilterFingerprint(filter operationHistoryFilter) string {
	canonical := fmt.Sprintf("kind=%s\nlane=%s\nstate=%s\n", filter.Kind, filter.Lane, filter.State)
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
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
		if definition.Lane == filter.Lane &&
			definition.HistoryAvailability == operations.HistoryAvailable {
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
	parsed, err := parseOperationRunsQuery(r, archiveUID)
	if err != nil {
		if errors.Is(err, errInvalidOperationCursor) {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "Operation history cursor is invalid")
			return
		}
		s.rejectBadParam(w, err)
		return
	}
	if parsed.filter.Kind != "" && operationKindUnavailable(parsed.filter.Kind) {
		writeOperationHistoryUnavailable(w)
		return
	}
	if parsed.filter.Lane != "" && len(parsed.Query.Kinds) == 0 {
		writeOperationHistoryUnavailable(w)
		return
	}

	runs, err := s.operationHistoryReader.ListRuns(r.Context(), parsed.Query)
	if err != nil {
		s.writeOperationHistoryReaderError(w, err)
		return
	}
	if len(runs) > parsed.Query.Limit+1 {
		writeError(w, http.StatusInternalServerError, "operation_history_failed",
			"Operation history could not be read")
		return
	}
	hasMore := len(runs) > parsed.Query.Limit
	if hasMore {
		runs = runs[:parsed.Query.Limit]
	}
	response := OperationRunsResponse{
		Runs:             make([]OperationRunSummary, 0, len(runs)),
		UnavailableKinds: unavailableOperationKinds(parsed.filter),
	}
	for _, run := range runs {
		summary, projectErr := operationRunSummary(run, archiveUID)
		if projectErr != nil {
			writeError(w, http.StatusInternalServerError, "operation_history_failed",
				"Operation history could not be read")
			return
		}
		response.Runs = append(response.Runs, summary)
	}
	if hasMore && len(runs) > 0 {
		last := runs[len(runs)-1]
		response.NextCursor, err = encodeOperationCursor(
			operations.Position{StartedAt: last.StartedAt, ID: last.ID}, parsed.filter, archiveUID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "operation_history_failed",
				"Operation history could not be read")
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
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
			HistoryAvailability: definition.HistoryAvailability,
			UnavailableCode:     definition.UnavailableCode,
			RelatedStatus:       operationRelatedStatus(definition.Kind),
			SupportedActions:    make([]operations.ActionID, 0),
		}
		switch definition.Kind {
		case operations.KindCardDAVSync:
			if lane.Configured {
				lane.SupportedActions = append(lane.SupportedActions, operations.ActionCardDAVSync)
			}
		case operations.KindVisualEmbedding:
			lane.SupportedActions = append(lane.SupportedActions, visualActions...)
		default:
			// The remaining registered lanes do not expose direct actions here.
		}
		if definition.HistoryAvailability == operations.HistoryAvailable {
			s.projectOperationLaneHistory(r.Context(), &lane)
		}
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
	lane.Active, err = operationRunSummaryPointer(status.Active, archiveUID)
	if err == nil {
		lane.Latest, err = operationRunSummaryPointer(status.Latest, archiveUID)
	}
	if err == nil {
		lane.LatestSuccessful, err = operationRunSummaryPointer(status.LatestSuccessful, archiveUID)
	}
	if err != nil {
		degradeOperationLaneHistory(lane)
	}
}

func operationRunSummaryPointer(run *operations.Run, archiveUID string) (*OperationRunSummary, error) {
	if run == nil {
		return nil, nil //nolint:nilnil // An absent lane run is a valid optional projection.
	}
	summary, err := operationRunSummary(*run, archiveUID)
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
		return s.cfg.Attachments.Documents.Enabled
	case operations.KindMessageEmbedding:
		return s.cfg.Vector.Enabled
	case operations.KindPersonEmbedding:
		return s.cfg.Vector.Enabled && s.cfg.Vector.People.Enabled
	case operations.KindPersonEnrichment:
		return false
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

func (s *Server) handleOperationRunDetail(w http.ResponseWriter, r *http.Request) {
	if s.operationHistoryReader == nil {
		writeOperationHistoryUnavailable(w)
		return
	}
	archiveUID, ok := s.operationHistoryArchiveUID(w, r)
	if !ok {
		return
	}
	id, err := decodeOperationRunReference(r.PathValue("id"), archiveUID)
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
	summary, err := operationRunSummary(run, archiveUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "operation_history_failed",
			"Operation history could not be read")
		return
	}
	writeJSON(w, http.StatusOK, OperationRunDetail{OperationRunSummary: summary})
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

func operationRunSummary(run operations.Run, archiveUID string) (OperationRunSummary, error) {
	if err := run.Validate(); err != nil {
		return OperationRunSummary{}, fmt.Errorf("project operation run: %w", err)
	}
	ref, err := encodeOperationRunReference(run.ID, archiveUID)
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

func operationKindUnavailable(kind operations.Kind) bool {
	for _, definition := range operations.LaneRegistry() {
		if definition.Kind == kind {
			return definition.HistoryAvailability == operations.HistoryUnavailable
		}
	}
	return true
}

func unavailableOperationKinds(filter operationHistoryFilter) []OperationUnavailableKind {
	result := make([]OperationUnavailableKind, 0)
	if filter.Kind != "" {
		return result
	}
	for _, definition := range operations.LaneRegistry() {
		if definition.HistoryAvailability != operations.HistoryUnavailable {
			continue
		}
		if filter.Lane != "" && definition.Lane != filter.Lane {
			continue
		}
		result = append(result, OperationUnavailableKind{
			Kind: definition.Kind, Lane: definition.Lane, UnavailableCode: definition.UnavailableCode,
		})
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

func encodeOperationToken(payload any) (string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode operation token: %w", err)
	}
	if len(encoded) > maxOperationTokenPayloadBytes {
		return "", errors.New("encode operation token: payload is too large")
	}
	return operationTokenVersion + "." + base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeOperationToken(raw string) ([]byte, error) {
	prefix, encoded, found := strings.Cut(raw, ".")
	if !found || prefix != operationTokenVersion || encoded == "" ||
		len(encoded) > base64.RawURLEncoding.EncodedLen(maxOperationTokenPayloadBytes) {
		return nil, errors.New("operation token has an invalid envelope")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(decoded) == 0 || len(decoded) > maxOperationTokenPayloadBytes {
		return nil, errors.New("operation token payload is invalid")
	}
	if base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return nil, errors.New("operation token payload encoding is noncanonical")
	}
	return decoded, nil
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
	case "kind", "id_type", "int_id", "string_id", "archive_uid":
		return true
	default:
		return false
	}
}

func operationCursorFieldAllowed(name string) bool {
	switch name {
	case "t", "k", "it", "i", "s", "f", "a":
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
