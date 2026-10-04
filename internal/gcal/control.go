package gcal

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"go.kenn.io/msgvault/internal/gmail"
)

// EventInput contains writable event fields. Pointers distinguish an omitted
// patch field from clearing it. Organizer identity is assigned by Google from
// the target calendar, never from the OAuth account.
type EventInput struct {
	ID               string         `json:"id,omitempty"`
	Summary          *string        `json:"summary,omitzero"`
	Description      *string        `json:"description,omitzero"`
	Location         *string        `json:"location,omitzero"`
	Start            *EventDateTime `json:"start,omitzero"`
	End              *EventDateTime `json:"end,omitzero"`
	Recurrence       *[]string      `json:"recurrence,omitzero"`
	Attendees        *[]Attendee    `json:"attendees,omitzero"`
	AttendeesOmitted *bool          `json:"attendeesOmitted,omitzero"`
	Reminders        *Reminders     `json:"reminders,omitzero"`
}

type Reminder struct {
	Method  string `json:"method"`
	Minutes int    `json:"minutes"`
}
type Reminders struct {
	UseDefault bool       `json:"useDefault"`
	Overrides  []Reminder `json:"overrides"`
}

type MutationOptions struct {
	SendUpdates string
	IfMatch     string
}

// ErrOutcomeUnknown means a provider mutation may have completed. Inspect the
// calendar before retrying; the underlying error can still be a context timeout.
var ErrOutcomeUnknown = errors.New("calendar request outcome unknown")

// PreconditionFailedError means the provider rejected a mutation because the
// event changed after it was read. Fetch the event again before applying it.
type PreconditionFailedError struct{}

func (*PreconditionFailedError) Error() string {
	return "calendar event changed; fetch it again before applying the update"
}

func SendUpdates(value string) (string, error) {
	switch value {
	case "", "none":
		return "none", nil
	case "all", "externalOnly":
		return value, nil
	default:
		return "", errors.New("send_updates must be all, externalOnly, or none")
	}
}

// ControlAPI extends the sync reader without requiring writes on sync fakes.
type ControlAPI interface {
	API
	InsertEvent(ctx context.Context, calendarID string, input EventInput, opts MutationOptions) (*Event, error)
	PatchEvent(ctx context.Context, calendarID, eventID string, input EventInput, opts MutationOptions) (*Event, error)
	DeleteEvent(ctx context.Context, calendarID, eventID string, opts MutationOptions) error
	MoveEvent(ctx context.Context, calendarID, eventID, destination string, opts MutationOptions) (*Event, error)
	ListInstances(ctx context.Context, calendarID, eventID string, params EventsListParams) (*EventsPage, error)
	FreeBusy(ctx context.Context, request FreeBusyRequest) (*FreeBusyResponse, error)
}

var _ ControlAPI = (*Client)(nil)

type FreeBusyItem struct {
	ID string `json:"id"`
}
type FreeBusyRequest struct {
	TimeMin  time.Time      `json:"timeMin"`
	TimeMax  time.Time      `json:"timeMax"`
	TimeZone string         `json:"timeZone,omitempty"`
	Items    []FreeBusyItem `json:"items"`
}
type BusyPeriod struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}
type CalendarError struct {
	Domain string `json:"domain,omitempty"`
	Reason string `json:"reason"`
}
type CalendarBusy struct {
	Busy   []BusyPeriod    `json:"busy"`
	Errors []CalendarError `json:"errors,omitempty"`
}
type FreeBusyResponse struct {
	Calendars map[string]CalendarBusy `json:"calendars"`
}

func eventPath(calendarID, eventID string) string {
	path := "/calendars/" + url.PathEscape(calendarID) + "/events"
	if eventID != "" {
		path += "/" + url.PathEscape(eventID)
	}
	return path
}

// controlRequest deliberately sends a mutation once. A lost response can mean
// the write succeeded; replaying a create or sending invitations again would
// be unsafe. Callers receive Google's error and can reconcile the event ID.
func (c *Client) controlRequest(ctx context.Context, method, path string, payload any, opts MutationOptions) ([]byte, error) {
	if err := c.rateLimiter.Acquire(ctx, gmail.OpEventsGet); err != nil {
		return nil, fmt.Errorf("calendar rate limit: %w", err)
	}
	var data []byte
	var err error
	if payload != nil {
		data, err = json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode calendar request: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if opts.IfMatch != "" {
		req.Header.Set("If-Match", opts.IfMatch)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if _, ok := errors.AsType[*tokenSourceFailureError](err); ok {
			return nil, fmt.Errorf("oauth token for calendar mutation: %w", err)
		}
		return nil, controlOutcomeError(path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, controlOutcomeError(path, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, &NotFoundError{Path: path}
	}
	if resp.StatusCode == http.StatusPreconditionFailed {
		return nil, &PreconditionFailedError{}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("calendar request failed (%d): %s", resp.StatusCode, body)
		if resp.StatusCode >= 500 {
			return nil, controlOutcomeError(path, err)
		}
		return nil, err
	}
	return body, nil
}

func controlOutcomeError(path string, err error) error {
	if path == "/freeBusy" {
		return fmt.Errorf("calendar availability request failed: %w", err)
	}
	return fmt.Errorf("%w; inspect the event before retrying: %w", ErrOutcomeUnknown, err)
}

func mutationPath(path string, opts MutationOptions) (string, error) {
	updates, err := SendUpdates(opts.SendUpdates)
	if err != nil {
		return "", err
	}
	return path + "?" + url.Values{"sendUpdates": {updates}}.Encode(), nil
}
func decodeControlEvent(body []byte) (*Event, error) {
	var wire wireEvent
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("%w; inspect the event before retrying; decode response: %w", ErrOutcomeUnknown, err)
	}
	ev := wire.toEvent()
	ev.Raw = append(ev.Raw[:0], body...)
	if ev.ID == "" {
		return nil, fmt.Errorf("%w; response missing event ID; inspect the calendar before retrying", ErrOutcomeUnknown)
	}
	return &ev, nil
}
func (c *Client) InsertEvent(ctx context.Context, calendarID string, input EventInput, opts MutationOptions) (*Event, error) {
	path, err := mutationPath(eventPath(calendarID, ""), opts)
	if err != nil {
		return nil, err
	}
	body, err := c.controlRequest(ctx, http.MethodPost, path, input, opts)
	if err != nil {
		return nil, err
	}
	return decodeControlEvent(body)
}
func (c *Client) PatchEvent(ctx context.Context, calendarID, eventID string, input EventInput, opts MutationOptions) (*Event, error) {
	path, err := mutationPath(eventPath(calendarID, eventID), opts)
	if err != nil {
		return nil, err
	}
	body, err := c.controlRequest(ctx, http.MethodPatch, path, input, opts)
	if err != nil {
		return nil, err
	}
	return decodeControlEvent(body)
}
func (c *Client) DeleteEvent(ctx context.Context, calendarID, eventID string, opts MutationOptions) error {
	path, err := mutationPath(eventPath(calendarID, eventID), opts)
	if err != nil {
		return err
	}
	_, err = c.controlRequest(ctx, http.MethodDelete, path, nil, opts)
	return err
}
func (c *Client) MoveEvent(ctx context.Context, calendarID, eventID, destination string, opts MutationOptions) (*Event, error) {
	path, err := mutationPath(eventPath(calendarID, eventID)+"/move", opts)
	if err != nil {
		return nil, err
	}
	path += "&" + url.Values{"destination": {destination}}.Encode()
	body, err := c.controlRequest(ctx, http.MethodPost, path, nil, opts)
	if err != nil {
		return nil, err
	}
	return decodeControlEvent(body)
}
func (c *Client) ListInstances(ctx context.Context, calendarID, eventID string, p EventsListParams) (*EventsPage, error) {
	v := url.Values{"showDeleted": {"false"}, "maxResults": {"2500"}}
	if p.TimeMin != "" {
		v.Set("timeMin", p.TimeMin)
	}
	if p.TimeMax != "" {
		v.Set("timeMax", p.TimeMax)
	}
	if p.PageToken != "" {
		v.Set("pageToken", p.PageToken)
	}
	body, err := c.request(ctx, gmail.OpEventsList, eventPath(calendarID, eventID)+"/instances?"+v.Encode())
	if err != nil {
		return nil, err
	}
	return decodeEventsPage(body)
}
func (c *Client) FreeBusy(ctx context.Context, input FreeBusyRequest) (*FreeBusyResponse, error) {
	if input.TimeMin.IsZero() || !input.TimeMax.After(input.TimeMin) || len(input.Items) == 0 || len(input.Items) > 50 {
		return nil, errors.New("freebusy requires an ordered time range and 1 to 50 calendars")
	}
	body, err := c.controlRequest(ctx, http.MethodPost, "/freeBusy", input, MutationOptions{})
	if err != nil {
		return nil, err
	}
	var result FreeBusyResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode freebusy: %w", err)
	}
	return &result, nil
}
