package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/peoplebrowser"
	"go.kenn.io/msgvault/internal/query"
)

type getPersonRelationshipResponse struct {
	ParticipantID    int64                                    `json:"participant_id"`
	CanonicalID      int64                                    `json:"canonical_id"`
	DisplayLabel     string                                   `json:"display_label"`
	Current          identityindex.TemperatureSummary         `json:"current"`
	Annual           []identityindex.AnnualTemperatureSummary `json:"annual"`
	PeakTemperature  int                                      `json:"peak_temperature"`
	PeakYear         int                                      `json:"peak_year"`
	ScoringTimezone  string                                   `json:"scoring_timezone"`
	ScoreVersion     int                                      `json:"score_version"`
	EffectiveDate    string                                   `json:"effective_date"`
	CacheRevision    string                                   `json:"cache_revision"`
	IdentityRevision int64                                    `json:"identity_revision"`
	Year             *int                                     `json:"year,omitempty"`
	Timezone         string                                   `json:"timezone,omitempty"`
	Days             []query.RelationshipCalendarDay          `json:"days,omitempty"`
}

func getPersonRelationshipDefinition(_ *handlers) toolDefinition {
	definition := readDefinition(
		ToolGetPersonRelationship,
		"Read graph-relative relationship temperature and optional daily activity for any observed contact. Temperature is inferred from archived interaction patterns; it is not emotional truth, consent, or authorization to contact the person.",
		closedObject(map[string]*jsonschema.Schema{
			toolArgParticipantID: safeIDSchema("Observed participant ID or canonical contact ID"),
			"year": boundedIntegerSchema(
				"Optional local calendar year; when omitted, daily rows are not returned", 1970, maxJSONSafeInteger,
			),
			"timezone": stringSchema("IANA timezone for calendar-day boundaries; defaults to UTC"),
		}, toolArgParticipantID),
		outputSchemaFor[getPersonRelationshipResponse](),
		func(h *handlers, ctx context.Context, req toolRequest) (*toolResult, error) {
			return h.getPersonRelationship(ctx, req)
		},
	)
	definition.availability = peopleAvailable
	return definition
}

func (h *handlers) getPersonRelationship(
	ctx context.Context, req toolRequest,
) (*toolResult, error) {
	args := req.GetArguments()
	participantID, err := requiredPeopleID(args, toolArgParticipantID)
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	timezone, _ := args["timezone"].(string)
	if timezone == "" {
		timezone = "UTC"
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return toolErrorResult(fmt.Sprintf("timezone must be a valid IANA timezone: %v", err)), nil
	}
	year, hasYear, err := optionalPeopleID(args, "year")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	if !hasYear {
		year = int64(time.Now().In(location).Year())
	}
	if year < 1970 || year > int64(time.Now().In(location).Year()) {
		return toolErrorResult("year must be between 1970 and the current local year"), nil
	}

	contact, err := h.peopleBackend.GetContact(ctx, participantID)
	if err != nil {
		if errors.Is(err, peoplebrowser.ErrContactNotFound) {
			return toolErrorResult("observed contact was not found"), nil
		}
		return nil, newInternalError("get person relationship contact", err)
	}
	if contact == nil {
		return nil, newInternalError("get person relationship contact", errors.New("empty response"))
	}
	calendar, err := h.peopleBackend.RelationshipCalendar(ctx, peoplebrowser.CalendarRequest{
		ParticipantID: participantID,
		Year:          int(year),
		Timezone:      timezone,
	})
	if err != nil {
		return nil, newInternalError("get person relationship", err)
	}
	if calendar == nil {
		return nil, newInternalError("get person relationship", errors.New("empty response"))
	}
	annual := calendar.Annual
	if annual == nil {
		annual = []identityindex.AnnualTemperatureSummary{}
	}
	response := getPersonRelationshipResponse{
		ParticipantID: participantID, CanonicalID: calendar.CanonicalID,
		DisplayLabel: contact.DisplayLabel, Current: calendar.Current, Annual: annual,
		PeakTemperature: calendar.PeakTemperature, PeakYear: calendar.PeakYear,
		ScoringTimezone: calendar.ScoringTimezone, ScoreVersion: calendar.ScoreVersion,
		EffectiveDate: calendar.EffectiveDate, CacheRevision: calendar.CacheRevision,
		IdentityRevision: calendar.IdentityRevision,
	}
	if hasYear {
		selectedYear := int(year)
		response.Year = &selectedYear
		response.Timezone = calendar.Timezone
		response.Days = calendar.Days
		if response.Days == nil {
			response.Days = []query.RelationshipCalendarDay{}
		}
	}
	return jsonResult(response)
}
