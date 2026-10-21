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

// ImportRequest accepts one supplied record or an archived Discord message ID,
// never credentials or database paths. Account is the stable source namespace.
// Bindings explicitly map native peer IDs or attendee
// email addresses to existing archive participant IDs.
type ImportRequest struct {
	Format   string           `json:"format"`
	Account  string           `json:"account"`
	Record   jsontext.Value   `json:"record"`
	Bindings map[string]int64 `json:"bindings,omitempty"`
}

func DecodeImport(r io.Reader) (Observation, error) {
	req, err := DecodeImportRequest(r)
	if err != nil {
		return Observation{}, err
	}
	return req.Observation()
}

// DecodeImportRequest validates supplied observations before transport while
// leaving exact archived-message references for the daemon to resolve.
func DecodeImportRequest(r io.Reader) (ImportRequest, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxRequestBytes+1))
	if err != nil {
		return ImportRequest{}, fmt.Errorf("read call import: %w", err)
	}
	if len(data) > MaxRequestBytes {
		return ImportRequest{}, ErrImportTooLarge
	}
	var req ImportRequest
	if err := json.Unmarshal(data, &req, json.RejectUnknownMembers(true)); err != nil {
		return ImportRequest{}, fmt.Errorf("%w: %w", ErrInvalid, ErrMalformedImport)
	}
	if req.Format == "discord_archived" {
		_, err = req.DiscordArchiveID()
	} else {
		_, err = req.Observation()
	}
	if err != nil {
		return ImportRequest{}, err
	}
	return req, nil
}

// DiscordArchiveID accepts an exact archive primary key, never a provider ID
// or path. Only the daemon may turn this reference into a native observation.
func (r ImportRequest) DiscordArchiveID() (int64, error) {
	var ref struct {
		MessageID int64 `json:"message_id"`
	}
	if len(r.Record) > MaxRawBytes {
		return 0, ErrImportTooLarge
	}
	if r.Format != "discord_archived" || !discordID(r.Account) || len(r.Bindings) > 100 {
		return 0, invalid("Discord archive account, record or bindings")
	}
	for native, id := range r.Bindings {
		if !discordID(native) || native == r.Account || id <= 0 {
			return 0, invalid("Discord binding")
		}
	}
	if json.Unmarshal(r.Record, &ref, json.RejectUnknownMembers(true)) != nil || ref.MessageID <= 0 {
		return 0, invalid("Discord archive requires a positive integer message_id")
	}
	return ref.MessageID, nil
}

func (r ImportRequest) Observation() (Observation, error) {
	if len(r.Record) > MaxRawBytes {
		return Observation{}, ErrImportTooLarge
	}
	if !validID(r.Account) || len(r.Record) == 0 || len(r.Bindings) > 100 {
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
	case "discord", "discord_chat_exporter":
		o, err = r.discordObservation()
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
