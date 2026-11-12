package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/apiimport"
	"go.kenn.io/msgvault/internal/messageimport"
)

const messageImportEndpointPath = "/api/v1/import/message"

type MessageImporter interface {
	ImportMessage(ctx context.Context, req messageimport.Request) (messageimport.Result, error)
}

type MessageImportResponse struct {
	Status          messageimport.Status `json:"status" enum:"created,updated"`
	SourceID        int64                `json:"source_id"`
	MessageID       int64                `json:"message_id"`
	SourceMessageID string               `json:"source_message_id"`
}

func (s *Server) registerMessageImportRoute(api huma.API) {
	registerImportRoute(s, api, importEndpoint[messageimport.Request, MessageImportResponse]{
		operationID:   "importMessage",
		path:          messageImportEndpointPath,
		summary:       "Import one message",
		noun:          "Message",
		hardenSchemas: hardenMessageImportSchemas,
		decode: func(r *http.Request) (messageimport.Request, error) {
			return messageimport.DecodeRequest(r.Body, messageimport.MaxRequestBytes)
		},
		validate: func(req messageimport.Request) error {
			_, err := req.Normalize()
			return err
		},
		importer: func(s *Server) (func(context.Context, messageimport.Request) (apiimport.Result, error), bool) {
			importer, ok := s.store.(MessageImporter)
			if !ok || importer == nil {
				return nil, false
			}
			return importer.ImportMessage, true
		},
		logFields: func(req messageimport.Request) []any {
			return []any{"source", req.Source.Identifier, "external_id", req.Message.ExternalID}
		},
		respond: func(result apiimport.Result) MessageImportResponse { return MessageImportResponse(result) },
	})
}

// hardenMessageImportSchemas states the cross-field rules that struct tags
// cannot: a sender needs one identifying field, a message needs a title or a
// body, and the body fields are mutually exclusive.
func hardenMessageImportSchemas(doc *huma.OpenAPI) {
	if doc == nil || doc.Components == nil || doc.Components.Schemas == nil {
		return
	}
	schemas := doc.Components.Schemas.Map()
	one := 1
	nonEmpty := func(field string) *huma.Schema {
		return &huma.Schema{
			Type:       huma.TypeObject,
			Required:   []string{field},
			Properties: map[string]*huma.Schema{field: {Type: huma.TypeString, MinLength: &one}},
		}
	}
	bothNonEmpty := func(left, right string) *huma.Schema {
		return &huma.Schema{
			Type:     huma.TypeObject,
			Required: []string{left, right},
			Properties: map[string]*huma.Schema{
				left:  {Type: huma.TypeString, MinLength: &one},
				right: {Type: huma.TypeString, MinLength: &one},
			},
		}
	}

	if sender := schemas["MessageImportSender"]; sender != nil {
		phoneLimit := 64
		if phone := sender.Properties["phone"]; phone != nil {
			phone.MaxLength = &phoneLimit
		}
		sender.AnyOf = []*huma.Schema{
			nonEmpty("name"), nonEmpty("url"), nonEmpty("email"), nonEmpty("phone"), nonEmpty("id"),
		}
	}
	message := schemas["ImportedMessage"]
	if message == nil {
		return
	}
	message.AllOf = []*huma.Schema{
		{AnyOf: []*huma.Schema{
			nonEmpty("title"), nonEmpty("body_markdown"), nonEmpty("body_html"), nonEmpty("body_text"),
		}},
		{Not: &huma.Schema{AnyOf: []*huma.Schema{
			bothNonEmpty("body_markdown", "body_html"),
			bothNonEmpty("body_markdown", "body_text"),
			bothNonEmpty("body_html", "body_text"),
		}}},
	}
}
