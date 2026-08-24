package mcp

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

type searchPeopleResponse struct {
	Rows          []searchPeopleRow `json:"rows"`
	TotalCount    int64             `json:"total_count"`
	NextCursor    string            `json:"next_cursor"`
	CacheRevision string            `json:"cache_revision"`
}

type searchPeopleRow struct {
	query.PersonSummary

	PersonID int64 `json:"person_id,omitempty"`
}

type searchPeopleCursor struct {
	Phase               string `json:"phase"`
	ProfileOffset       int    `json:"profile_offset,omitempty"`
	ObservedCursor      string `json:"observed_cursor,omitempty"`
	Query               string `json:"query"`
	Limit               int    `json:"limit"`
	ProfilesFingerprint string `json:"profiles_fingerprint"`
}

const searchPeopleCursorPrefix = "msgvault-people-v1:"

type getPersonNotesResponse struct {
	PersonID  int64            `json:"person_id"`
	Text      string           `json:"text"`
	ValueID   int64            `json:"value_id"`
	Source    store.Provenance `json:"source,omitempty"`
	UpdatedAt *time.Time       `json:"updated_at"`
	Exists    bool             `json:"exists"`
}

type updatePersonNotesResponse struct {
	Current    *store.PersonAttributeValue `json:"current"`
	Superseded *store.PersonAttributeValue `json:"superseded"`
}

func searchPeopleDefinition(_ *handlers) toolDefinition {
	definition := readDefinition(
		ToolSearchPeople,
		"Search observed contacts and durable people profiles without returning private profile attributes.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgQuery:  stringSchema("Optional identity or display-name query"),
			toolArgLimit:  nonNegativeIntegerSchema("Maximum results to return (default 20)", 20),
			toolArgCursor: stringSchema("Opaque cursor from a previous search_people response"),
		}),
		outputSchemaFor[searchPeopleResponse](),
		func(h *handlers, ctx context.Context, req toolRequest) (*toolResult, error) {
			return h.searchPeople(ctx, req)
		},
	)
	definition.availability = peopleAvailable
	return definition
}

func getPersonNotesDefinition(_ *handlers) toolDefinition {
	definition := readDefinition(
		ToolGetPersonNotes,
		"Read the private Notes value and its provenance for a durable person profile.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgPersonID: safeIDSchema("Durable person profile ID"),
		}, toolArgPersonID),
		outputSchemaFor[getPersonNotesResponse](),
		func(h *handlers, ctx context.Context, req toolRequest) (*toolResult, error) {
			return h.getPersonNotes(ctx, req)
		},
	)
	definition.availability = peopleAvailable
	return definition
}

func promotePersonDefinition(_ *handlers) toolDefinition {
	definition := profileWriteDefinition(
		ToolPromotePerson,
		"Explicitly promote an observed participant identity cluster to a durable person profile.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgParticipantID: safeIDSchema("Observed participant ID to promote"),
		}, toolArgParticipantID),
		outputSchemaFor[store.Person](),
		func(h *handlers, ctx context.Context, req toolRequest) (*toolResult, error) {
			return h.promotePerson(ctx, req)
		},
	)
	definition.availability = peopleAvailable
	return definition
}

func updatePersonNotesDefinition(_ *handlers) toolDefinition {
	mode := stringSchema("Update mode: append atomically or replace with compare-and-swap", "append", "replace")
	mode.Default = []byte(`"append"`)
	definition := profileWriteDefinition(
		ToolUpdatePersonNotes,
		"Update private Notes for a durable person. MCP writes use enrichment provenance. Observed contacts must be promoted explicitly first.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgPersonID: safeIDSchema("Durable person profile ID"),
			bodyFormatText:  stringSchema("Non-blank note text; multiline UTF-8 is preserved"),
			toolArgMode:     mode,
			"expected_value_id": safeIDSchema(
				"Required for replace when Notes exist; forbidden for append or first-value creation"),
		}, toolArgPersonID, bodyFormatText),
		outputSchemaFor[updatePersonNotesResponse](),
		func(h *handlers, ctx context.Context, req toolRequest) (*toolResult, error) {
			return h.updatePersonNotes(ctx, req)
		},
	)
	definition.availability = peopleAvailable
	return definition
}

func (h *handlers) searchPeople(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	queryText, _ := args[toolArgQuery].(string)
	queryText = strings.TrimSpace(queryText)
	rawCursor, _ := args[toolArgCursor].(string)
	limit := limitArg(args, toolArgLimit, defaultSearchLimit)
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	lister, ok := h.peopleBackend.(peoplebrowser.ProfileLister)
	if !ok {
		return nil, newInternalError("search people", errors.New("durable profile listing is unavailable"))
	}
	profiles, err := lister.ListProfiles(ctx)
	if err != nil {
		return nil, newInternalError("list durable people", err)
	}
	prepared := h.prepareCuratedPeopleSearch(ctx, profiles, queryText)
	cursor, err := decodeSearchPeopleCursor(rawCursor, queryText, limit, prepared.fingerprint)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	if cursor.ProfileOffset > len(prepared.rows) {
		return toolErrorResult("search_people cursor is out of range; restart the search"), nil
	}
	if cursor.Phase == "profiles" && len(prepared.rows) > 0 {
		page, pageErr := h.peopleBackend.Search(ctx, peoplebrowser.SearchRequest{
			Query: queryText, Limit: limit,
		})
		if pageErr != nil {
			return nil, newInternalError("search observed people", pageErr)
		}
		if page == nil {
			return nil, newInternalError("search observed people", errors.New("empty response"))
		}
		end := min(cursor.ProfileOffset+limit, len(prepared.rows))
		rows := append([]searchPeopleRow(nil), prepared.rows[cursor.ProfileOffset:end]...)
		for i := range rows {
			rows[i].CacheRevision = page.CacheRevision
		}
		nextCursor := ""
		switch {
		case end < len(prepared.rows):
			cursor.ProfileOffset = end
			nextCursor, err = encodeSearchPeopleCursor(cursor)
		case page.TotalCount > int64(len(prepared.excludedIDs)):
			cursor.Phase = "observed"
			cursor.ProfileOffset = 0
			nextCursor, err = encodeSearchPeopleCursor(cursor)
		}
		if err != nil {
			return nil, newInternalError("encode search people cursor", err)
		}
		return jsonResult(searchPeopleResponse{
			Rows: rows, TotalCount: mergedPeopleTotal(page.TotalCount, prepared),
			NextCursor: nextCursor, CacheRevision: page.CacheRevision,
		})
	}

	page, rows, err := h.searchObservedPeoplePage(ctx, queryText, limit, cursor.ObservedCursor, prepared)
	if err != nil {
		return nil, newInternalError("search observed people", err)
	}
	nextCursor := ""
	if page.NextCursor != "" {
		if !prepared.hasProfiles {
			nextCursor = page.NextCursor
		} else {
			cursor.Phase = "observed"
			cursor.ObservedCursor = page.NextCursor
			nextCursor, err = encodeSearchPeopleCursor(cursor)
			if err != nil {
				return nil, newInternalError("encode search people cursor", err)
			}
		}
	}
	return jsonResult(searchPeopleResponse{
		Rows: rows, TotalCount: mergedPeopleTotal(page.TotalCount, prepared),
		NextCursor: nextCursor, CacheRevision: page.CacheRevision,
	})
}

type curatedPeopleSearch struct {
	rows          []searchPeopleRow
	byParticipant map[int64]store.Person
	excludedIDs   map[int64]struct{}
	fingerprint   string
	hasProfiles   bool
}

func (h *handlers) prepareCuratedPeopleSearch(
	ctx context.Context, profiles []store.Person, queryText string,
) curatedPeopleSearch {
	slices.SortFunc(profiles, func(a, b store.Person) int {
		aLabel, bLabel := profileDisplayLabel(a), profileDisplayLabel(b)
		if order := strings.Compare(strings.ToLower(aLabel), strings.ToLower(bLabel)); order != 0 {
			return order
		}
		return cmp.Compare(a.ID, b.ID)
	})
	prepared := curatedPeopleSearch{
		rows:          []searchPeopleRow{},
		byParticipant: make(map[int64]store.Person),
		excludedIDs:   make(map[int64]struct{}),
		fingerprint:   peopleProfilesFingerprint(profiles),
		hasProfiles:   len(profiles) > 0,
	}
	for _, profile := range profiles {
		for _, participantID := range profile.ParticipantIDs {
			prepared.byParticipant[participantID] = profile
		}
		if !profileMatchesPeopleQuery(profile, queryText) {
			continue
		}
		summary := query.PersonSummary{
			Identifiers: []query.PersonIdentifier{}, SourceCounts: []query.SourceCount{},
		}
		for _, participantID := range profile.ParticipantIDs {
			contact, contactErr := h.peopleBackend.GetContact(ctx, participantID)
			if contactErr != nil || contact == nil {
				continue
			}
			if summary.ID == 0 {
				summary = *contact
			}
			canonicalID := contact.ID
			if contact.Cluster != nil {
				canonicalID = contact.Cluster.CanonicalID
			}
			if personSummaryMatchesPeopleQuery(*contact, queryText) {
				prepared.excludedIDs[canonicalID] = struct{}{}
			}
		}
		if summary.ID == 0 && len(profile.ParticipantIDs) > 0 {
			summary.ID = slices.Min(profile.ParticipantIDs)
		}
		applyProfileToPeopleSummary(&summary, profile)
		prepared.rows = append(prepared.rows, searchPeopleRow{
			PersonSummary: summary, PersonID: profile.ID,
		})
	}
	return prepared
}

func (h *handlers) searchObservedPeoplePage(
	ctx context.Context, queryText string, limit int, cursor string, prepared curatedPeopleSearch,
) (*peoplebrowser.SearchPage, []searchPeopleRow, error) {
	for {
		page, err := h.peopleBackend.Search(ctx, peoplebrowser.SearchRequest{
			Query: queryText, Limit: limit, Cursor: cursor,
		})
		if err != nil {
			return nil, nil, err
		}
		if page == nil {
			return nil, nil, errors.New("empty response")
		}
		rows := make([]searchPeopleRow, 0, len(page.Rows))
		for _, summary := range page.Rows {
			if _, excluded := prepared.excludedIDs[summary.ID]; excluded {
				continue
			}
			row := searchPeopleRow{PersonSummary: summary}
			if profile, exists := profileForPeopleSummary(summary, prepared.byParticipant); exists {
				applyProfileToPeopleSummary(&row.PersonSummary, profile)
				row.PersonID = profile.ID
			}
			rows = append(rows, row)
		}
		if len(rows) > 0 || page.NextCursor == "" {
			return page, rows, nil
		}
		cursor = page.NextCursor
	}
}

func decodeSearchPeopleCursor(
	raw, queryText string, limit int, fingerprint string,
) (searchPeopleCursor, error) {
	initial := searchPeopleCursor{
		Phase: "profiles", Query: queryText, Limit: limit,
		ProfilesFingerprint: fingerprint,
	}
	if raw == "" {
		return initial, nil
	}
	if !strings.HasPrefix(raw, searchPeopleCursorPrefix) {
		initial.Phase = "observed"
		initial.ObservedCursor = raw
		return initial, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, searchPeopleCursorPrefix))
	if err != nil {
		return searchPeopleCursor{}, errors.New("search_people cursor is invalid; restart the search")
	}
	var cursor searchPeopleCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return searchPeopleCursor{}, errors.New("search_people cursor is invalid; restart the search")
	}
	if (cursor.Phase != "profiles" && cursor.Phase != "observed") ||
		cursor.ProfileOffset < 0 || cursor.Query != queryText || cursor.Limit != limit {
		return searchPeopleCursor{}, errors.New("search_people cursor does not match this search; restart the search")
	}
	if cursor.ProfilesFingerprint != fingerprint {
		return searchPeopleCursor{}, errors.New("durable people changed during pagination; restart the search")
	}
	return cursor, nil
}

func encodeSearchPeopleCursor(cursor searchPeopleCursor) (string, error) {
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return searchPeopleCursorPrefix + base64.RawURLEncoding.EncodeToString(data), nil
}

func profileDisplayLabel(profile store.Person) string {
	if profile.DisplayName != nil {
		if label := strings.TrimSpace(*profile.DisplayName); label != "" {
			return label
		}
	}
	return profile.VCardUID
}

func profileMatchesPeopleQuery(profile store.Person, queryText string) bool {
	queryText = strings.ToLower(strings.TrimSpace(queryText))
	if queryText == "" {
		return true
	}
	return strings.Contains(strings.ToLower(profileDisplayLabel(profile)), queryText) ||
		strings.Contains(strings.ToLower(profile.VCardUID), queryText)
}

func personSummaryMatchesPeopleQuery(summary query.PersonSummary, queryText string) bool {
	queryText = strings.ToLower(strings.TrimSpace(queryText))
	if queryText == "" {
		return true
	}
	values := []string{summary.DisplayLabel, summary.DisplayName}
	for _, identifier := range summary.Identifiers {
		values = append(values, identifier.Value, identifier.DisplayValue)
	}
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), queryText) {
			return true
		}
	}
	return false
}

func peopleProfilesFingerprint(profiles []store.Person) string {
	hash := sha256.New()
	for _, profile := range profiles {
		_, _ = fmt.Fprintf(hash, "%d\x00%s\x00%d\x00", profile.ID, profile.VCardUID, profile.Revision)
		if profile.DisplayName != nil {
			_, _ = fmt.Fprintf(hash, "%s", *profile.DisplayName)
		}
		for _, participantID := range profile.ParticipantIDs {
			_, _ = fmt.Fprintf(hash, "\x00%d", participantID)
		}
		_, _ = hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func applyProfileToPeopleSummary(summary *query.PersonSummary, profile store.Person) {
	if profile.DisplayName != nil {
		if displayName := strings.TrimSpace(*profile.DisplayName); displayName != "" {
			summary.DisplayLabel = displayName
			summary.DisplayName = displayName
		}
	}
	if summary.DisplayLabel == "" {
		summary.DisplayLabel = profile.VCardUID
	}
	if summary.Identifiers == nil {
		summary.Identifiers = []query.PersonIdentifier{}
	}
	if summary.SourceCounts == nil {
		summary.SourceCounts = []query.SourceCount{}
	}
	summary.Profile = &query.PersonProfile{
		ID: profile.ID, DisplayName: profile.DisplayName, Revision: profile.Revision,
	}
}

func profileForPeopleSummary(
	summary query.PersonSummary, profiles map[int64]store.Person,
) (store.Person, bool) {
	if profile, ok := profiles[summary.ID]; ok {
		return profile, true
	}
	if summary.Cluster != nil {
		for _, participantID := range summary.Cluster.MemberIDs {
			if profile, ok := profiles[participantID]; ok {
				return profile, true
			}
		}
	}
	for _, identifier := range summary.Identifiers {
		if profile, ok := profiles[identifier.ParticipantID]; ok {
			return profile, true
		}
	}
	return store.Person{}, false
}

func mergedPeopleTotal(observedTotal int64, prepared curatedPeopleSearch) int64 {
	total := observedTotal - int64(len(prepared.excludedIDs)) + int64(len(prepared.rows))
	if total < int64(len(prepared.rows)) {
		return int64(len(prepared.rows))
	}
	return total
}

func (h *handlers) getPersonNotes(ctx context.Context, req toolRequest) (*toolResult, error) {
	personID, err := requiredPeopleID(req.GetArguments(), toolArgPersonID)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	attributes, err := h.peopleBackend.ListAttributes(ctx, personID)
	if err != nil {
		if personProfileMissing(err) {
			return toolErrorResult(explicitPromotionMessage), nil
		}
		return nil, newInternalError("get person notes", err)
	}
	response := getPersonNotesResponse{PersonID: personID}
	_, current := notesAttribute(attributes)
	if current != nil {
		if current.Value.Text == nil {
			return nil, newInternalError("get person notes", errors.New("notes value is not text"))
		}
		response.Exists = true
		response.Text = *current.Value.Text
		response.ValueID = current.ID
		response.Source = current.Source
		updatedAt := current.CreatedAt
		response.UpdatedAt = &updatedAt
	}
	return jsonResult(response)
}

func (h *handlers) promotePerson(ctx context.Context, req toolRequest) (*toolResult, error) {
	participantID, err := requiredPeopleID(req.GetArguments(), toolArgParticipantID)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	person, err := h.peopleBackend.Promote(ctx, participantID)
	if err != nil {
		return nil, newInternalError("promote person", err)
	}
	if person == nil {
		return nil, newInternalError("promote person", errors.New("empty response"))
	}
	return jsonResult(person)
}

func (h *handlers) updatePersonNotes(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	personID, err := requiredPeopleID(args, toolArgPersonID)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	text, _ := args[bodyFormatText].(string)
	if strings.TrimSpace(text) == "" {
		return toolErrorResult("text must not be blank"), nil
	}
	mode, _ := args[toolArgMode].(string)
	if mode == "" {
		mode = "append"
	}
	expectedValueID, hasExpectedValueID, err := optionalPeopleID(args, "expected_value_id")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}

	var write *store.PersonAttributeWrite
	switch mode {
	case "append":
		if hasExpectedValueID {
			return toolErrorResult("expected_value_id is not allowed with append"), nil
		}
		if _, err = h.peopleBackend.ListAttributes(ctx, personID); err != nil {
			break
		}
		write, err = h.peopleBackend.AppendNote(ctx, peoplebrowser.AppendNoteRequest{
			PersonID: personID, Text: text,
			Source: store.ProvenanceEnrichment, Actor: "mcp",
		})
	case "replace":
		attributes, listErr := h.peopleBackend.ListAttributes(ctx, personID)
		if listErr != nil {
			err = listErr
			break
		}
		group, current := notesAttribute(attributes)
		if group == nil {
			return nil, newInternalError("update person notes",
				errors.New("notes attribute definition is unavailable"))
		}
		switch {
		case current != nil && !hasExpectedValueID:
			return toolErrorResult(
				"expected_value_id is required when replacing existing notes"), nil
		case current == nil && hasExpectedValueID:
			return toolErrorResult(
				"expected_value_id must be omitted when creating the first notes value"), nil
		}
		var expected *int64
		if hasExpectedValueID {
			expected = &expectedValueID
		}
		write, err = h.peopleBackend.SetAttribute(ctx, peoplebrowser.SetAttributeRequest{
			PersonID: personID, Slug: group.Definition.Slug,
			Value:           store.AttributeValue{Type: store.AttributeValueText, Text: &text},
			ExpectedValueID: expected,
			Source:          store.ProvenanceEnrichment,
			Actor:           "mcp",
		})
	default:
		return toolErrorResult("mode must be append or replace"), nil
	}
	if err != nil {
		if personProfileMissing(err) {
			return toolErrorResult(explicitPromotionMessage), nil
		}
		if stale, ok := errors.AsType[peoplebrowser.StaleValueError](err); ok {
			if stale.CurrentValueID == 0 {
				return toolErrorResult(
					"notes changed or were removed; reload before retrying"), nil
			}
			return toolErrorResult(fmt.Sprintf(
				"notes changed; reload and retry with expected_value_id %d",
				stale.CurrentValueID,
			)), nil
		}
		return nil, newInternalError("update person notes", err)
	}
	if write == nil {
		return nil, newInternalError("update person notes", errors.New("empty response"))
	}
	return jsonResult(updatePersonNotesResponse{
		Current: write.Value, Superseded: write.Superseded,
	})
}

func requiredPeopleID(args map[string]any, key string) (int64, error) {
	value, err := positiveInt64Arg(args, key)
	if err != nil {
		return 0, err
	}
	if value == 0 {
		return 0, fmt.Errorf("%s is required", key)
	}
	return value, nil
}

func optionalPeopleID(args map[string]any, key string) (int64, bool, error) {
	if _, ok := args[key]; !ok {
		return 0, false, nil
	}
	value, err := positiveInt64Arg(args, key)
	return value, true, err
}

func notesAttribute(attributes *peoplebrowser.Attributes) (
	*peoplebrowser.AttributeGroup, *store.PersonAttributeValue,
) {
	if attributes == nil {
		return nil, nil
	}
	for i := range attributes.Groups {
		group := &attributes.Groups[i]
		if group.Definition.UniversalID != store.AttributeUniversalIDNotes {
			continue
		}
		if len(group.Current) == 0 {
			return group, nil
		}
		return group, &group.Current[0]
	}
	return nil, nil
}

const explicitPromotionMessage = "person profile not found; invoke promote_person with the observed participant_id before updating notes"

func personProfileMissing(err error) bool {
	var coded daemonAPIErrorCoder
	return errors.As(err, &coded) && coded.APIErrorCode() == "person_profile_not_found"
}
