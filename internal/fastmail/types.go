package fastmail

import (
	"encoding/json"
	"fmt"
)

const (
	CoreCapability        = "urn:ietf:params:jmap:core"
	SubmissionCapability  = "urn:ietf:params:jmap:submission"
	MaskedEmailCapability = "https://www.fastmail.com/dev/maskedemail"
)

// Record is one provider-reported address that can supply identity evidence.
type Record struct {
	Identifier string
	State      string
	Kind       string
}

// CapabilityError reports that an explicit provider operation cannot proceed
// because the JMAP session does not advertise a required capability.
type CapabilityError struct {
	Capability string
}

func (e *CapabilityError) Error() string {
	return "Fastmail JMAP capability unavailable: " + e.Capability
}

// ObjectLimitError reports that a JMAP /get call covered more objects than the
// server allows in one method call (RFC 8620 "requestTooLarge"). The
// MaskedEmail extension has no /query or /changes method, so the inventory
// cannot be enumerated in chunks; accounts with more records than the
// session's maxObjectsInGet cannot be listed. MaxObjectsInGet is zero when the
// session did not advertise the limit.
type ObjectLimitError struct {
	Method          string
	MaxObjectsInGet int64
}

func (e *ObjectLimitError) Error() string {
	if e.MaxObjectsInGet > 0 {
		return fmt.Sprintf(
			"%s exceeded the JMAP server limit of %d objects per call (requestTooLarge)",
			e.Method,
			e.MaxObjectsInGet,
		)
	}
	return e.Method + " exceeded the JMAP server object limit (requestTooLarge)"
}

type sessionResponse struct {
	APIURL          string                     `json:"apiUrl"`
	Capabilities    map[string]json.RawMessage `json:"capabilities"`
	Accounts        map[string]sessionAccount  `json:"accounts"`
	PrimaryAccounts map[string]string          `json:"primaryAccounts"`
}

type sessionAccount struct {
	AccountCapabilities map[string]json.RawMessage `json:"accountCapabilities"`
}

type jmapRequest struct {
	Using       []string            `json:"using"`
	MethodCalls [][]json.RawMessage `json:"methodCalls"`
}

type jmapResponse struct {
	MethodResponses []json.RawMessage `json:"methodResponses"`
}

type maskedEmailGetResponse struct {
	AccountID string `json:"accountId"`
	List      []struct {
		Email string `json:"email"`
		State string `json:"state"`
	} `json:"list"`
}

type identityGetResponse struct {
	AccountID string `json:"accountId"`
	List      []struct {
		Email string `json:"email"`
	} `json:"list"`
}
