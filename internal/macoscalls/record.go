// Package macoscalls reads client-side macOS Phone and FaceTime evidence.
package macoscalls

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"math/big"
	"time"

	"go.kenn.io/msgvault/internal/callhistory"
)

type record map[string]any

// The allowlist also fixes the raw evidence keys when optional columns are absent.
var columns = []struct{ name, kind, label string }{
	{"ZUNIQUE_ID", "text", "identifier"}, {"ZDATE", "number", "date"}, {"ZDURATION", "number", "duration"},
	{"ZORIGINATED", "integer", "direction"}, {"ZANSWERED", "integer", "answered status"}, {"ZADDRESS", "text", "address"},
	{"ZNAME", "text", "name"}, {"ZHANDLE_TYPE", "integer", "handle type"}, {"ZSERVICE_PROVIDER", "text", "service provider"},
	{"ZCALLTYPE", "integer", "type"}, {"ZPARTICIPANTGROUPUUID", "text", "participant group"},
	{"ZORIGINATINGDEVICENAME", "text", "originating device name"}, {"ZDISCONNECTED_CAUSE", "integer", "disconnect cause"},
}

func (r record) observation(account string) (callhistory.Observation, error) {
	fail := func(field string) (callhistory.Observation, error) {
		return callhistory.Observation{}, fmt.Errorf("invalid call %s", field)
	}
	for _, c := range columns {
		v := r[c.name]
		if v == nil {
			continue
		}
		valid := false
		switch v.(type) {
		case string:
			valid = c.kind == "text"
		case int64:
			valid = c.kind == "number" || c.kind == "integer"
		case float64:
			valid = c.kind == "number"
		}
		if !valid {
			return fail(c.label)
		}
	}
	number := func(v any) *float64 {
		switch n := v.(type) {
		case int64:
			f := float64(n)
			return &f
		case float64:
			return &n
		}
		return nil
	}
	date := number(r["ZDATE"])
	if date == nil || math.IsNaN(*date) || math.IsInf(*date, 0) || *date < -63113904000 || *date >= 252423993600 {
		return fail("date")
	}
	whole, fraction := math.Modf(*date)
	// Keep enough precision that a value just below a half-nanosecond
	// cannot round up to the tie during scaling.
	nanos := new(big.Float).SetPrec(128).SetFloat64(fraction)
	nanos.Mul(nanos, big.NewFloat(1e9))
	nanos.Add(nanos, big.NewFloat(math.Copysign(0.5, fraction)))
	rounded, _ := nanos.Int64()
	at := time.Unix(int64(whole)+978307200, rounded).UTC()
	duration := number(r["ZDURATION"])
	if duration != nil && (*duration < 0 || math.IsNaN(*duration) || math.IsInf(*duration, 0)) {
		return fail("duration")
	}
	raw := make(record, len(columns))
	for _, c := range columns {
		raw[c.name] = r[c.name]
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return fail("raw evidence")
	}
	id, _ := r["ZUNIQUE_ID"].(string)
	o := callhistory.Observation{
		RecordID: id, Raw: data,
		Version: 1, Provider: "macos", Account: account,
		Direction: "unknown", Outcome: "unknown", OwnerParticipation: "unknown", OccurredAt: at,
	}
	switch r["ZORIGINATED"] {
	case int64(0):
		o.Direction = "incoming"
	case int64(1):
		o.Direction = "outgoing"
	}
	switch r["ZANSWERED"] {
	case int64(0):
		o.OwnerParticipation = "not_joined"
	case int64(1):
		o.Outcome, o.OwnerParticipation = "answered", "joined"
		o.DurationSeconds = duration
	}
	if err := o.Validate(); err != nil {
		return callhistory.Observation{}, err
	}
	return o, nil
}
