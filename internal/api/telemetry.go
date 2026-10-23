package api

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

const telemetryEventsPath = "/api/v1/telemetry/events"

// TelemetryEventRequest documents the body kit's capture handler accepts.
type TelemetryEventRequest struct {
	Event      string         `json:"event" doc:"Allowlisted event name, app_opened, screen_viewed, session_ended, search_run, message_opened, sync_finished, search_empty, or error_shown"`
	Properties map[string]any `json:"properties,omitempty" doc:"Fixed properties: surface (web, tui, cli), screen for screen_viewed, duration_bucket for session_ended, search_type (text, semantic, hybrid) for search_run. Searches report after successful current first-page results load, including retries and reloads; message opens report when readable. Core actions require surface; search_run also requires search_type. The daemon counts successful manual browser sync completions even after the person leaves Sources; the CLI posts its successful sync_finished events. search_empty counts submitted empty searches once per UTC day, surface, and search type, excluding previews and reloads. Everything sends no search_empty while recording search is unknown or set up. error_shown requires surface and kind (search, message, sync, other) for displayed search, archive load, message, or Sync now requests on Sources that fail to start, without daily deduplication; retries can send repeats. sync_failed fires when a scheduled sync or Sync now on Sources finds a source starting to fail; it is daemon-only and rejected here. Manual CLI syncs and Settings → CardDAV Sync now neither record nor clear outages; the next successful scheduled pass clears them. Unlisted outcome values become other. Invalid core actions are dropped; unknown properties are stripped"`
}

// TelemetryEventResponse documents kit's accepted response.
type TelemetryEventResponse struct {
	Status string `json:"status" enum:"queued,disabled" doc:"queued when accepted, including suppressed daily screen_viewed and message_opened duplicates or invalid screen names; disabled when telemetry is off"`
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
