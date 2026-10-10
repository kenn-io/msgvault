package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"net/http"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/identitycontrol"
	"go.kenn.io/msgvault/internal/store"
)

const identityOperationsPath = "/api/v1/identity/operations"

var errIdentityOperationDenied = errors.New("current principal does not authorize every affected identity resource")

type identityOperationStore interface {
	IdentityOperationPreviewContext(ctx context.Context, operation identitycontrol.Operation, target identitycontrol.IdentityTarget) (*store.IdentitySnapshot, error)
	ApplyIdentityOperationWithPreviewContext(ctx context.Context, request store.IdentityOperationRequest, authorize, verifyPreview func(context.Context, *store.IdentitySnapshot) error) (*store.IdentityReceipt, error)
	IdentityOperationReceiptContext(ctx context.Context, principal, key string) (*store.IdentityReceipt, error)
	IdentityReceiptByIDContext(ctx context.Context, id string) (*store.IdentityReceipt, error)
}

type identityOperationPreviewResponse struct {
	Snapshot     *store.IdentitySnapshot `json:"snapshot"`
	PreviewToken string                  `json:"preview_token"`
	ExpiresAt    time.Time               `json:"expires_at"`
}

type identityOperationApplyResponse struct {
	store.IdentityReceipt

	CacheState string `json:"cache_state" enum:"ready,stale,unknown"`
}

type identityOperationApplyRequest struct {
	Operation           identitycontrol.Operation      `json:"operation" enum:"graph-link,graph-unlink,person-link,person-unlink"`
	Target              identitycontrol.IdentityTarget `json:"target"`
	ExpectedFingerprint string                         `json:"expected_fingerprint"`
	PreviewToken        string                         `json:"preview_token"`
	IdempotencyKey      string                         `json:"idempotency_key"`
}

func (s *Server) registerIdentityOperationRoutes(api huma.API) {
	preview := rawAPIV1Operation("previewIdentityOperation", http.MethodPost, "/identity/operations/preview", "Preview one explicit identity operation without mutation")
	preview.RequestBody = jsonRequestBodyFor[identitycontrol.PreviewRequest](api)
	preview.Responses = jsonResponsesFor[identityOperationPreviewResponse](api)
	preview.Errors = []int{400, 401, 403, 404, 413, 415, 500, 501, 503}
	addErrorResponses(api, preview.Responses, preview.Errors...)
	registerRawHumaRoute(api, preview, s.handleIdentityOperationPreview)
	apply := rawAPIV1Operation("applyIdentityOperation", http.MethodPost, "/identity/operations/apply", "Apply one signed identity preview or recover its exact committed retry")
	apply.RequestBody = jsonRequestBodyFor[identityOperationApplyRequest](api)
	apply.Responses = jsonResponsesFor[identityOperationApplyResponse](api)
	apply.Errors = []int{400, 401, 403, 404, 409, 413, 415, 500, 501, 503, 504}
	addErrorResponses(api, apply.Responses, apply.Errors...)
	registerRawHumaRoute(api, apply, s.handleIdentityOperationApply)
	receipt := rawAPIV1Operation("getIdentityOperationReceipt", http.MethodGet, "/identity/operations/receipt", "Read one committed identity outcome without replaying its mutation")
	receipt.Parameters = []*huma.Param{queryStringParam("idempotency_key", "Exact key for this principal", false), queryStringParam("receipt_id", "Exact receipt ID; owner only", false), queryStringParam("principal", "Exact prior principal for owner recovery", false)}
	receipt.Responses = jsonResponsesFor[store.IdentityReceipt](api)
	receipt.Errors = []int{400, 401, 403, 404, 500, 501, 503}
	addErrorResponses(api, receipt.Responses, receipt.Errors...)
	registerRawHumaRoute(api, receipt, s.handleIdentityOperationReceipt)
}

func (s *Server) handleIdentityOperationPreview(w http.ResponseWriter, r *http.Request) {
	var request identitycontrol.PreviewRequest
	if !decodeIdentityOperationJSON(w, r, &request) {
		return
	}
	if err := request.Validate(); err != nil {
		s.writeIdentityOperationError(w, err)
		return
	}
	auth := s.classifyAPIRequestDirect(r)
	principal, err := identityOperationPrincipal(auth)
	if err != nil {
		s.writeIdentityOperationError(w, err)
		return
	}
	if auth.Mode == AuthModeDelegated && !auth.Grant.HasPermission(agentgrant.PermissionIdentityRead) {
		s.writeIdentityOperationError(w, errIdentityOperationDenied)
		return
	}
	backend, ok := s.store.(identityOperationStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "identity_operations_unavailable", "Native identity operation control is unavailable")
		return
	}
	snapshot, err := backend.IdentityOperationPreviewContext(r.Context(), request.Operation, request.Target)
	if err != nil {
		s.writeIdentityOperationError(w, err)
		return
	}
	// Scope admission precedes disclosure of native names and ownership evidence.
	current := s.classifyAPIRequestDirect(r)
	currentPrincipal, err := identityOperationPrincipal(current)
	if err != nil || currentPrincipal != principal {
		s.writeIdentityOperationError(w, errIdentityOperationDenied)
		return
	}
	if err = authorizeIdentityOperationSnapshot(current, snapshot, agentgrant.PermissionIdentityRead); err != nil {
		s.writeIdentityOperationError(w, err)
		return
	}
	authority, err := identityOperationAuthority(current)
	if err != nil {
		s.writeIdentityOperationError(w, err)
		return
	}
	now := time.Now().UTC()
	expires := now.Add(identitycontrol.PreviewTTL)
	token, err := identitycontrol.SignPreview(s.exploreCursorKey[:], identitycontrol.PreviewClaims{Binding: identitycontrol.PreviewBinding{PrincipalID: principal, AuthorityFingerprint: authority, Request: request, Fingerprint: snapshot.Fingerprint}, IssuedAt: now, ExpiresAt: expires})
	if err != nil {
		s.writeIdentityOperationError(w, err)
		return
	}
	// The daemon's random key is purpose-separated by the identity token domain.
	// Restart invalidates pending previews; committed receipt recovery is independent.
	writeJSON(w, http.StatusOK, identityOperationPreviewResponse{Snapshot: snapshot, PreviewToken: token, ExpiresAt: expires})
}

func (s *Server) handleIdentityOperationApply(w http.ResponseWriter, r *http.Request) {
	var request identityOperationApplyRequest
	if !decodeIdentityOperationJSON(w, r, &request) {
		return
	}
	auth := s.classifyAPIRequestDirect(r)
	principal, err := identityOperationPrincipal(auth)
	if err != nil {
		s.writeIdentityOperationError(w, err)
		return
	}
	backend, ok := s.store.(identityOperationStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "identity_operations_unavailable", "Native identity operation control is unavailable")
		return
	}
	action := agentgrant.PermissionIdentityLink
	if request.Operation == identitycontrol.OperationGraphUnlink || request.Operation == identitycontrol.OperationPersonUnlink {
		action = agentgrant.PermissionIdentityUnlink
	}
	if auth.Mode == AuthModeDelegated && (!auth.Grant.HasPermission(agentgrant.PermissionIdentityRead) || !auth.Grant.HasPermission(action)) {
		s.writeIdentityOperationError(w, errIdentityOperationDenied)
		return
	}
	authorize := func(_ context.Context, snapshot *store.IdentitySnapshot) error {
		current := s.classifyAPIRequestDirect(r)
		id, err := identityOperationPrincipal(current)
		if err != nil || id != principal {
			return errIdentityOperationDenied
		}
		return authorizeIdentityOperationSnapshot(current, snapshot, action)
	}
	// Denied callers never wait for or observe the daemon's write gate. This
	// read-only admission is repeated under Store's identity fence below.
	admission, err := backend.IdentityOperationPreviewContext(r.Context(), request.Operation, request.Target)
	if err != nil {
		s.writeIdentityOperationError(w, err)
		return
	}
	if err := authorize(r.Context(), admission); err != nil {
		s.writeIdentityOperationError(w, err)
		return
	}
	if s.operationGate != nil {
		release, ok := beginGateWorkBounded(r.Context(), s.operationGate, "identity operation")
		if !ok {
			writeOperationGateBusy(w, r, s.operationGate)
			return
		}
		defer release()
	}
	freshWrite := false
	verify := func(_ context.Context, _ *store.IdentitySnapshot) error {
		current := s.classifyAPIRequestDirect(r)
		id, err := identityOperationPrincipal(current)
		if err != nil || id != principal {
			return errIdentityOperationDenied
		}
		authority, err := identityOperationAuthority(current)
		if err != nil {
			return err
		}
		// Bind to the submitted preview digest. Store checks current native evidence
		// separately, after its authorized committed-receipt lookup.
		if err := identitycontrol.VerifyPreview(s.exploreCursorKey[:], request.PreviewToken, identitycontrol.PreviewBinding{PrincipalID: principal, AuthorityFingerprint: authority, Request: identitycontrol.PreviewRequest{Operation: request.Operation, Target: request.Target}, Fingerprint: request.ExpectedFingerprint}, time.Now().UTC()); err != nil {
			return err
		}
		freshWrite = true
		return nil
	}
	receipt, err := backend.ApplyIdentityOperationWithPreviewContext(r.Context(), store.IdentityOperationRequest{Principal: principal, IdempotencyKey: request.IdempotencyKey, Operation: request.Operation, Target: request.Target, ExpectedFingerprint: request.ExpectedFingerprint}, authorize, verify)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			writeError(w, http.StatusGatewayTimeout, "identity_outcome_unknown", "Identity outcome is unknown; read the receipt with the same key before retrying")
			return
		}
		s.writeIdentityOperationError(w, err)
		return
	}
	cacheState := "unknown"
	if freshWrite && receipt.Changed {
		cacheState = s.refreshIdentityCacheState(r.Context())
	}
	writeJSON(w, http.StatusOK, identityOperationApplyResponse{IdentityReceipt: *receipt, CacheState: cacheState})
}

func (s *Server) handleIdentityOperationReceipt(w http.ResponseWriter, r *http.Request) {
	auth := s.classifyAPIRequestDirect(r)
	principal, err := identityOperationPrincipal(auth)
	if err != nil {
		s.writeIdentityOperationError(w, err)
		return
	}
	if auth.Mode == AuthModeDelegated && !auth.Grant.HasPermission(agentgrant.PermissionIdentityRead) {
		s.writeIdentityOperationError(w, errIdentityOperationDenied)
		return
	}
	values := r.URL.Query()
	for name, entries := range values {
		if (name != "idempotency_key" && name != "receipt_id" && name != "principal") || len(entries) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_identity_request", "Select one exact receipt lookup")
			return
		}
	}
	key, id, selectedPrincipal := values.Get("idempotency_key"), values.Get("receipt_id"), values.Get("principal")
	if auth.Mode == AuthModeDelegated && (id != "" || selectedPrincipal != "") {
		s.writeIdentityOperationError(w, errIdentityOperationDenied)
		return
	}
	if (key == "") == (id == "") || (id != "" && selectedPrincipal != "") {
		writeError(w, http.StatusBadRequest, "invalid_identity_request", "Select one exact receipt lookup")
		return
	}
	if selectedPrincipal != "" {
		principal = selectedPrincipal
	}
	backend, ok := s.store.(identityOperationStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "identity_operations_unavailable", "Native identity operation control is unavailable")
		return
	}
	var receipt *store.IdentityReceipt
	if id != "" {
		receipt, err = backend.IdentityReceiptByIDContext(r.Context(), id)
	} else {
		receipt, err = backend.IdentityOperationReceiptContext(r.Context(), principal, key)
	}
	if err != nil {
		s.writeIdentityOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func identityOperationPrincipal(auth requestAuthentication) (string, error) {
	switch auth.Mode {
	case AuthModeAPIKey, AuthModeSession, AuthModeLoopback:
		return "owner", nil
	case AuthModeDelegated:
		if auth.Grant != nil && auth.Grant.ID != "" {
			return auth.Grant.ID, nil
		}
	case AuthModeRequired:
		return "", errIdentityOperationDenied
	}
	return "", errIdentityOperationDenied
}

func identityOperationAuthority(auth requestAuthentication) (string, error) {
	var encoded []byte
	if auth.Mode == AuthModeDelegated {
		if auth.Grant == nil {
			return "", errIdentityOperationDenied
		}
		var err error
		encoded, err = json.Marshal(auth.Grant, json.Deterministic(true))
		if err != nil {
			return "", err
		}
	} else {
		encoded = []byte("msgvault-owner:v1")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func authorizeIdentityOperationSnapshot(auth requestAuthentication, snapshot *store.IdentitySnapshot, action agentgrant.Permission) error {
	if auth.Mode != AuthModeDelegated {
		_, err := identityOperationPrincipal(auth)
		return err
	}
	grant := auth.Grant
	if grant == nil || !grant.HasPermission(agentgrant.PermissionIdentityRead) || !grant.HasPermission(action) {
		return errIdentityOperationDenied
	}
	sourceIDs := make(map[int64]bool, len(snapshot.Sources))
	for _, source := range snapshot.Sources {
		ref := agentgrant.SourceRef{ID: source.ID, Type: source.Type, Identifier: source.Identifier}
		if !grant.Allows(agentgrant.PermissionIdentityRead, ref) || !grant.Allows(action, ref) {
			return errIdentityOperationDenied
		}
		sourceIDs[source.ID] = true
	}
	personIDs := make(map[int64]bool, len(snapshot.Persons))
	for _, person := range snapshot.Persons {
		ref := agentgrant.PersonRef{ID: person.ID, UID: person.UID}
		if !grant.AllowsPerson(agentgrant.PermissionIdentityRead, ref) || !grant.AllowsPerson(action, ref) {
			return errIdentityOperationDenied
		}
		personIDs[person.ID] = true
	}
	accountOwners := make(map[int64]string, len(snapshot.Accounts))
	for _, account := range snapshot.Accounts {
		accountOwners[account.ID] = account.OwnershipFingerprint
	}
	for _, book := range snapshot.Books {
		ref := agentgrant.AddressBookRef{AccountID: book.AccountID, BookID: book.ID, CanonicalURL: book.CanonicalURL, OwnershipFingerprint: accountOwners[book.AccountID]}
		if !grant.AllowsAddressBook(agentgrant.PermissionIdentityRead, ref) || !grant.AllowsAddressBook(action, ref) {
			return errIdentityOperationDenied
		}
	}
	covered := make(map[int64]bool, len(snapshot.Members))
	for _, occurrence := range snapshot.SourceContributions {
		if !sourceIDs[occurrence.SourceID] {
			return errIdentityOperationDenied
		}
		covered[occurrence.ParticipantID] = true
	}
	// A manually curated member with no imported source requires exact native
	// person membership and the durable UID grant. Imported sources never bypass
	// the complete source checks above, and CardDAV ownership also requires books.
	for _, binding := range snapshot.Bindings {
		if personIDs[binding.PersonID] {
			covered[binding.ParticipantID] = true
		}
	}
	for _, member := range snapshot.Members {
		if !covered[member] {
			return errIdentityOperationDenied
		}
	}
	return nil
}

func decodeIdentityOperationJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != applicationJSONMediaType {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 16*1024+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_identity_request", "Invalid identity request JSON")
		return false
	}
	if len(data) > 16*1024 {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Identity request exceeds 16 KiB")
		return false
	}
	var fields map[string]jsontext.Value
	if err = json.Unmarshal(data, &fields); err != nil || fields == nil || meetingJSONContainsNull(fields) || meetingJSONHasNoncanonicalField(fields, reflect.TypeOf(destination)) {
		writeError(w, http.StatusBadRequest, "invalid_identity_request", "Use one non-null JSON object with canonical identity fields")
		return false
	}
	if err = json.Unmarshal(data, destination, json.RejectUnknownMembers(true)); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_identity_request", "Invalid identity request JSON")
		return false
	}
	return true
}

func (s *Server) writeIdentityOperationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errIdentityOperationDenied):
		writeError(w, http.StatusForbidden, "identity_denied", errIdentityOperationDenied.Error())
	case errors.Is(err, identitycontrol.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, "invalid_identity_request", err.Error())
	case errors.Is(err, identitycontrol.ErrInvalidPreview), errors.Is(err, store.ErrIdentityOperationIdempotency), errors.Is(err, store.ErrIdentityOperationStale), errors.Is(err, store.ErrIdentityOperationBlocked):
		writeError(w, http.StatusConflict, "identity_conflict", err.Error())
	case errors.Is(err, store.ErrIdentityOperationTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "identity_scope_too_large", err.Error())
	case errors.Is(err, store.ErrIdentityOperationReceiptNotFound), errors.Is(err, store.ErrPersonNotFound), errors.Is(err, store.ErrParticipantNotFound):
		writeError(w, http.StatusNotFound, "identity_not_found", "Requested identity resource or receipt was not found")
	default:
		if s.writeIfContextError(w, err) {
			return
		}
		s.logger.Error("identity operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "identity_operation_failed", "Could not complete the identity operation")
	}
}
