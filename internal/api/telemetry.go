package api

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

const telemetryEventsPath = "/api/v1/telemetry/events"

// TelemetryEventRequest documents the body kit's capture handler accepts.
type TelemetryEventRequest struct {
	Event      string         `json:"event" doc:"Allowlisted interface event name"`
	Properties map[string]any `json:"properties,omitempty" doc:"Allowlisted event properties; unknown properties are stripped"`
}

// TelemetryEventResponse documents kit's accepted response.
type TelemetryEventResponse struct {
	Status string `json:"status" enum:"queued,disabled" doc:"queued when accepted; disabled otherwise"`
}

func (s *Server) registerTelemetryRoutes(apiV1 huma.API) {
	op := rawAPIV1Operation("captureTelemetryEvent", http.MethodPost, "/telemetry/events", "Record an interface event")
	op.RequestBody = jsonRequestBodyFor[TelemetryEventRequest](apiV1)
	op.Responses = jsonResponsesFor[TelemetryEventResponse](apiV1, http.StatusAccepted)
	// kit writes plain-text errors and the auth layer writes ErrorResponse, so the error body stays undeclared.
	op.Responses["default"] = &huma.Response{Description: "Error"}
	// A closure, because the OpenAPI document registers routes on a bare Server.
	registerRawHumaRoute(apiV1, op, func(w http.ResponseWriter, r *http.Request) { s.telemetryCapture.ServeHTTP(w, r) })
}
