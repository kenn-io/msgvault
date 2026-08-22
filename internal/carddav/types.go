// Package carddav provides the standards-only CardDAV client used by msgvault.
package carddav

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"
)

var (
	ErrUnsafeRedirect  = errors.New("carddav redirect leaves credential origin")
	ErrUnsafeHref      = errors.New("carddav href leaves collection")
	ErrUnsafeTarget    = errors.New("carddav target is unsafe")
	ErrUnsafeXML       = errors.New("unsafe XML")
	ErrXMLLimit        = errors.New("XML limit exceeded")
	ErrResponseLimit   = errors.New("CardDAV response exceeds byte limit")
	ErrOperationLimit  = errors.New("CardDAV operation exceeds byte limit")
	ErrInvalidProperty = errors.New("unsupported DAV property")
)

const (
	davNamespace     = "DAV:"
	cardDAVNamespace = "urn:ietf:params:xml:ns:carddav"
)

const (
	defaultRequestTimeout   = 30 * time.Second
	defaultOperationTimeout = 5 * time.Minute
	defaultResponseBytes    = 32 << 20
	defaultOperationBytes   = 256 << 20
)

// ClientOptions controls bounded, authenticated DAV requests.
type ClientOptions struct {
	CredentialOrigin *url.URL
	Username         string
	Password         string
	RequestTimeout   time.Duration
	OperationTimeout time.Duration
	ResponseBytes    int64
	OperationBytes   int64
	Resolver         *net.Resolver
	DialContext      func(context.Context, string, string) (net.Conn, error)
	// AllowInsecureCredentials permits Basic authentication over HTTP. It is
	// intended only for controlled test fixtures; production callers must use
	// the zero value so credentials require HTTPS.
	AllowInsecureCredentials bool
}

// Request is one DAV request. ETag selects If-Match unless Create selects
// If-None-Match: * instead.
type Request struct {
	Method string
	URL    string
	Depth  *int
	Body   []byte
	ETag   string
	Create bool
}

// Response contains an already-bounded response body.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	// transferredBytes includes bounded bodies consumed from redirect hops as
	// well as Body so callers can enforce budgets spanning multiple requests.
	transferredBytes int64
	// EffectiveURL is the final validated same-origin URL after redirects.
	EffectiveURL *url.URL
}

type operationBudget struct {
	remaining int64
}

func (b *operationBudget) consume(response *Response) error {
	consumed := response.transferredBytes
	if consumed == 0 {
		consumed = int64(len(response.Body))
	}
	b.remaining -= consumed
	if b.remaining < 0 {
		return ErrOperationLimit
	}
	return nil
}

// StatusError represents an HTTP error response that callers can branch on
// without parsing strings. RetryAfter is populated for 429 responses when the
// server supplied a valid value, clamped to one hour.
type StatusError struct {
	StatusCode   int
	RetryAfter   time.Duration
	Precondition string
}

func (e *StatusError) Error() string { return http.StatusText(e.StatusCode) }

// XMLLimits bounds untrusted DAV XML before it is unmarshaled.
type XMLLimits struct {
	MaxBytes     int64
	MaxDepth     int
	MaxElements  int
	MaxResponses int
	MaxPropStats int
}

// DefaultXMLLimits returns the CardDAV operation's per-response XML budget.
func DefaultXMLLimits() XMLLimits {
	return XMLLimits{
		MaxBytes: defaultResponseBytes, MaxDepth: 64,
		MaxElements: 500_000, MaxResponses: 50_001, MaxPropStats: 100_000,
	}
}

// MultiStatus is a parsed DAV multistatus response.
type MultiStatus struct {
	Responses []MultiStatusResponse
	SyncToken string
}

type MultiStatusResponse struct {
	Href       string
	StatusCode int
	PropStats  []PropStat
}

type PropStat struct {
	StatusCode int
	Properties Properties
}

// Properties preserves the DAV/CardDAV properties needed by discovery and
// synchronization without retaining unbounded arbitrary XML.
type Properties struct {
	GetETag              string
	AddressData          string
	SyncToken            string
	DisplayName          string
	CurrentUserPrincipal string
	AddressbookHomeSet   []string
	ResourceTypePresent  bool
	IsCollection         bool
	IsAddressBook        bool
	PrivilegesPresent    bool
	Privileges           []string
	SupportsSync         bool
	SupportsMultiget     bool
	SupportedVCard       []string
}

// PropertyName identifies a DAV XML property.
type PropertyName struct {
	Namespace string
	Local     string
}

var (
	CurrentUserPrincipalProperty  = PropertyName{Namespace: davNamespace, Local: "current-user-principal"}
	AddressbookHomeSetProperty    = PropertyName{Namespace: cardDAVNamespace, Local: "addressbook-home-set"}
	GetETagProperty               = PropertyName{Namespace: davNamespace, Local: "getetag"}
	AddressDataProperty           = PropertyName{Namespace: cardDAVNamespace, Local: "address-data"}
	DisplayNameProperty           = PropertyName{Namespace: davNamespace, Local: "displayname"}
	SyncTokenProperty             = PropertyName{Namespace: davNamespace, Local: "sync-token"}
	ResourceTypeProperty          = PropertyName{Namespace: davNamespace, Local: "resourcetype"}
	SupportedReportSetProperty    = PropertyName{Namespace: davNamespace, Local: "supported-report-set"}
	CurrentUserPrivilegesProperty = PropertyName{Namespace: davNamespace, Local: "current-user-privilege-set"}
	SupportedAddressDataProperty  = PropertyName{Namespace: cardDAVNamespace, Local: "supported-address-data"}
)
