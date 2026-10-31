package store

import "encoding/json/v2"

const messageTypeCalendarEvent = "calendar_event"

// CalendarProjection is the calendar state that calendar sync stores in a
// calendar event's metadata. A field is nil when the stored event does not
// carry it, such as a cancellation recorded before any details were seen.
type CalendarProjection struct {
	Status   *string `json:"status"`
	Sequence *int64  `json:"sequence"`
	Start    *string `json:"start"`
	End      *string `json:"end"`
	AllDay   *bool   `json:"all_day"`
	TimeZone *string `json:"time_zone"`
	ICalUID  *string `json:"ical_uid"`
}

// ParseCalendarProjection reads the calendar state from calendar event
// metadata. Unreadable metadata yields an empty projection. Without a start
// time, all_day is unknown too, so it is cleared.
func ParseCalendarProjection(metadata string) *CalendarProjection {
	projection := &CalendarProjection{}
	if err := json.Unmarshal([]byte(metadata), projection); err != nil {
		return &CalendarProjection{}
	}
	if projection.Start == nil || *projection.Start == "" {
		projection.Start = nil
		projection.AllDay = nil
	}
	return projection
}
