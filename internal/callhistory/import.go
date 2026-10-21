package callhistory

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
)

var ErrImportTooLarge = errors.New("call import exceeds size limit")
var ErrMalformedImport = errors.New("malformed call import JSON")

// ImportRequest accepts one supplied record, never credentials or database
// paths. Account is the stable source namespace: a Signal account identifier
// or a Google calendar ID. Bindings explicitly map native peer IDs or attendee
// email addresses to existing archive participant IDs.
type ImportRequest struct {
	Format   string           `json:"format"`
	Account  string           `json:"account"`
	Record   jsontext.Value   `json:"record"`
	Bindings map[string]int64 `json:"bindings,omitempty"`
}

func DecodeImport(r io.Reader) (Observation, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxRequestBytes+1))
	if err != nil {
		return Observation{}, fmt.Errorf("read call import: %w", err)
	}
	if len(data) > MaxRequestBytes {
		return Observation{}, ErrImportTooLarge
	}
	var req ImportRequest
	if err := json.Unmarshal(data, &req, json.RejectUnknownMembers(true)); err != nil {
		return Observation{}, fmt.Errorf("%w: %w", ErrInvalid, ErrMalformedImport)
	}
	return req.Observation()
}

func (r ImportRequest) Observation() (Observation, error) {
	if !validID(r.Account) || len(r.Record) == 0 || len(r.Record) > MaxRawBytes || len(r.Bindings) > 100 {
		return Observation{}, invalid("account, record or bindings")
	}
	for native, id := range r.Bindings {
		if !validID(native) || id <= 0 {
			return Observation{}, invalid("binding")
		}
	}
	var o Observation
	var err error
	switch r.Format {
	case "normalized":
		if len(r.Bindings) != 0 {
			return o, invalid("normalized records contain their own participants")
		}
		if err = json.Unmarshal(r.Record, &o, json.RejectUnknownMembers(true)); err != nil {
			return o, invalid("normalized record")
		}
		if o.Account != r.Account {
			return o, invalid("account mismatch")
		}
	case "signal_desktop":
		o, err = r.signalObservation()
	case "google_calendar":
		o, err = r.calendarObservation()
	default:
		err = invalid("unsupported format")
	}
	if err != nil {
		return Observation{}, err
	}
	if err := o.Validate(); err != nil {
		return Observation{}, err
	}
	return o, nil
}
