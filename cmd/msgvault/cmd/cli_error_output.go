package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
)

// cliError separates a diagnostic from its recovery guidance, retaining causes.
type cliError struct {
	Kind     string
	Code     string
	Message  string
	Hint     string
	ExitCode int
	Err      error
	Details  jsontext.Value
}

func (e *cliError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Code
}
func (e *cliError) Unwrap() error { return e.Err }

func renderCLIError(w io.Writer, err *cliError, jsonMode bool) {
	if jsonMode {
		record := cliErrorRecord{Code: err.Code, Message: err.Error(), Hint: err.Hint, ExitCode: err.ExitCode, Details: err.Details}
		_ = json.MarshalWrite(w, struct {
			Error cliErrorRecord `json:"error"`
		}{record}, jsontext.AllowInvalidUTF8(true))
		_, _ = fmt.Fprintln(w)
		return
	}
	_, _ = fmt.Fprintln(w, "Error:", err.Error())
	if len(err.Details) > 0 {
		details := string(err.Details)
		if err.Details[0] == '"' {
			var decoded string
			if json.Unmarshal(err.Details, &decoded) == nil {
				details = decoded
			}
		}
		_, _ = fmt.Fprintln(w, details)
	}
	if err.Hint != "" {
		_, _ = fmt.Fprintln(w, "hint:", err.Hint)
	}
}

type cliErrorRecord struct {
	Code     string         `json:"code"`
	Message  string         `json:"message"`
	Hint     string         `json:"hint,omitempty"`
	ExitCode int            `json:"exit_code"`
	Details  jsontext.Value `json:"details,omitempty"`
}
