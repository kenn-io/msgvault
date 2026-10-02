package fastmail

import (
	"encoding/json/jsontext"
	"fmt"
)

const (
	CoreCapability        = "urn:ietf:params:jmap:core"
	SubmissionCapability  = "urn:ietf:params:jmap:submission"
	MaskedEmailCapability = "https://www.fastmail.com/dev/maskedemail"
)

// Record is one provider-reported address that can supply identity evidence.
type Record struct {
	ID            string
	AccountID     string
	ForDomain     string
	Description   string
	CreatedAt     string
	LastMessageAt string
	Identifier    string
	State         string
	Kind          string
}

// Snapshot is a complete inventory with an opaque composite JMAP state.
// An empty State means the server did not provide all collection states.
type Snapshot struct {
	Records []Record
	State   string
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
	APIURL          string                    `json:"apiUrl"`
	Capabilities    map[string]jsontext.Value `json:"capabilities"`
	Accounts        map[string]sessionAccount `json:"accounts"`
	PrimaryAccounts map[string]string         `json:"primaryAccounts"`
}

type sessionAccount struct {
	AccountCapabilities map[string]jsontext.Value `json:"accountCapabilities"`
}

type jmapRequest struct {
	Using       []string           `json:"using"`
	MethodCalls [][]jsontext.Value `json:"methodCalls"`
}

type jmapResponse struct {
	MethodResponses []jsontext.Value `json:"methodResponses"`
}

type maskedEmailGetResponse struct {
	State     string   `json:"state"`
	NotFound  []string `json:"notFound"`
	AccountID string   `json:"accountId"`
	List      []struct {
		ID            string `json:"id"`
		Email         string `json:"email"`
		State         string `json:"state"`
		ForDomain     string `json:"forDomain"`
		Description   string `json:"description"`
		CreatedAt     string `json:"createdAt"`
		LastMessageAt string `json:"lastMessageAt"`
	} `json:"list"`
}

type identityGetResponse struct {
	State     string   `json:"state"`
	NotFound  []string `json:"notFound"`
	AccountID string   `json:"accountId"`
	List      []struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"list"`
}
