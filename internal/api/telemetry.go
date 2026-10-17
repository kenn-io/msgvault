package api

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

const telemetryEventsPath = "/api/v1/telemetry/events"

// TelemetryEventRequest documents the body kit's capture handler accepts.
type TelemetryEventRequest struct {
	Event      string         `json:"event" doc:"Allowlisted event name, app_opened, screen_viewed, session_ended, search_run, message_opened, sync_finished, search_empty, or error_shown"`
	Properties map[string]any `json:"properties,omitempty" doc:"Fixed properties: surface (web, tui, cli), screen for screen_viewed, duration_bucket for session_ended, search_type (text, semantic, hybrid) for search_run. Searches report after successful current first-page results load, including retries and reloads; message opens report when readable. Core actions require surface; search_run also requires search_type. The daemon counts successful manual browser sync completions even after the person leaves Sources; the CLI posts its successful sync_finished events. search_empty uses search_type after a settled empty first page; error_shown uses kind (auth, not_found, rate_limited, provider, network, other), with unknown kinds normalized to other. These outcomes have no daily suppression; callers claim retries locally. sync_failed is daemon-owned and cannot be posted. Invalid core actions are dropped; unknown properties are stripped"`
}

// TelemetryEventResponse documents kit's accepted response.
type TelemetryEventResponse struct {
	Status string `json:"status" enum:"queued,disabled" doc:"queued when accepted, including suppressed daily screen duplicates or invalid screen names; disabled when telemetry is off"`
}

func (s *Server) registerTelemetryRoutes(apiV1 huma.API) {
	op := rawAPIV1Operation("captureTelemetryEvent", http.MethodPost, "/telemetry/events", "Report a web, terminal UI, or interactive CLI usage event")
	op.RequestBody = jsonRequestBodyFor[TelemetryEventRequest](apiV1)
	op.Responses = jsonResponsesFor[TelemetryEventResponse](apiV1, http.StatusAccepted)
	// kit writes plain-text errors and the auth layer writes ErrorResponse, so the error body stays undeclared.
	op.Responses["default"] = &huma.Response{Description: "Error"}
	// A closure, because the OpenAPI document registers routes on a bare Server.
	registerRawHumaRoute(apiV1, op, func(w http.ResponseWriter, r *http.Request) { s.telemetryCapture.ServeHTTP(w, r) })
}
