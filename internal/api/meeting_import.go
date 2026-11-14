package api

import (
	"context"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/apiimport"
	"go.kenn.io/msgvault/internal/meetingimport"
)

const meetingImportEndpointPath = "/api/v1/import/meeting"

type MeetingImporter interface {
	ImportMeeting(ctx context.Context, req meetingimport.Request) (meetingimport.Result, error)
}

type MeetingImportResponse struct {
	Status          meetingimport.Status `json:"status" enum:"created,updated"`
	SourceID        int64                `json:"source_id"`
	MessageID       int64                `json:"message_id"`
	SourceMessageID string               `json:"source_message_id"`
}

const statusFieldName = "status"

func (s *Server) registerMeetingImportRoute(api huma.API) {
	registerImportRoute(s, api, importEndpoint[meetingimport.Request, MeetingImportResponse]{
		operationID:   "importMeeting",
		path:          meetingImportEndpointPath,
		summary:       "Import one meeting",
		noun:          "Meeting",
		hardenSchemas: hardenMeetingImportSchemas,
		decode: func(r *http.Request) (meetingimport.Request, error) {
			return meetingimport.DecodeRequest(r.Body, meetingimport.MaxRequestBytes)
		},
		validate: func(req meetingimport.Request) error {
			_, err := req.Normalize()
			return err
		},
		importer: func(s *Server) (func(context.Context, meetingimport.Request) (apiimport.Result, error), bool) {
			importer, ok := s.store.(MeetingImporter)
			if !ok || importer == nil {
				return nil, false
			}
			return importer.ImportMeeting, true
		},
		logFields: func(req meetingimport.Request) []any {
			return []any{"source", req.Source.Identifier, "external_id", req.Meeting.ExternalID}
		},
		respond: func(result apiimport.Result) MeetingImportResponse { return MeetingImportResponse(result) },
	})
}

func hardenMeetingImportSchemas(doc *huma.OpenAPI) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	meeting := doc.Components.Schemas.Map()["Meeting"]
	if meeting == nil {
		return
	}
	if actions := meeting.Properties["action_items"]; actions != nil {
		actions.Nullable = false
		maximum := 1000
		actions.MaxItems = &maximum
	}
	action := doc.Components.Schemas.Map()["MeetingActionItem"]
	if action != nil {
		minimum := 1
		for property, maximum := range map[string]int{
			"title": 4096, "description": 65536, "source_id": 256,
			"assignee_name": 256, statusFieldName: 128, "due_date": 256,
		} {
			if field := action.Properties[property]; field != nil {
				limit := maximum
				field.MaxLength = &limit
				if property == "title" {
					field.MinLength = &minimum
				}
			}
		}
		if email := action.Properties["assignee_email"]; email != nil {
			email.Format = "email"
		}
	}
	if person := doc.Components.Schemas.Map()["MeetingPerson"]; person != nil {
		idLimit, phoneLimit := 200, 64
		if id := person.Properties["id"]; id != nil {
			id.MaxLength = &idLimit
		}
		if phone := person.Properties["phone"]; phone != nil {
			phone.MaxLength = &phoneLimit
		}
		person.Required = slices.DeleteFunc(person.Required, func(field string) bool { return field == "email" })
		person.AnyOf = []*huma.Schema{
			{
				Type: huma.TypeObject, Required: []string{"email"},
				Properties: map[string]*huma.Schema{"email": {Type: huma.TypeString, Format: "email"}},
			},
			{
				Type: huma.TypeObject, Required: []string{"phone"},
				Properties: map[string]*huma.Schema{"phone": {Type: huma.TypeString, MaxLength: &phoneLimit}},
			},
		}
	}
	one := 1
	contentRequired := []*huma.Schema{
		{
			Type:     huma.TypeObject,
			Required: []string{"summary_markdown"},
			Properties: map[string]*huma.Schema{
				"summary_markdown": {Type: huma.TypeString, MinLength: &one},
			},
		},
		{
			Type:     huma.TypeObject,
			Required: []string{"summary_text"},
			Properties: map[string]*huma.Schema{
				"summary_text": {Type: huma.TypeString, MinLength: &one},
			},
		},
		{
			Type:     huma.TypeObject,
			Required: []string{"transcript"},
			Properties: map[string]*huma.Schema{
				"transcript": {Type: huma.TypeString, MinLength: &one},
			},
		},
		{
			Type:     huma.TypeObject,
			Required: []string{"transcript_segments"},
			Properties: map[string]*huma.Schema{
				"transcript_segments": {Type: huma.TypeArray, MinItems: &one},
			},
		},
	}
	transcriptsExclusive := &huma.Schema{
		Type:     huma.TypeObject,
		Required: []string{"transcript", "transcript_segments"},
		Properties: map[string]*huma.Schema{
			"transcript":          {Type: huma.TypeString, MinLength: &one},
			"transcript_segments": {Type: huma.TypeArray, MinItems: &one},
		},
	}
	meeting.AllOf = []*huma.Schema{
		{AnyOf: contentRequired},
		{Not: transcriptsExclusive},
	}
}
