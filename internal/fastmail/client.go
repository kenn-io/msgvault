package fastmail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	defaultSessionURL     = "https://api.fastmail.com/jmap/session"
	defaultRequestTimeout = 30 * time.Second
	maxResponseBytes      = 8 << 20
	maskedEmailGet        = "MaskedEmail/get"

	maskedCallID   = "masked"
	identityCallID = "identity"
)

// Client reads identity inventory metadata from Fastmail's JMAP endpoint.
type Client struct {
	apiToken   string
	httpClient *http.Client
	sessionURL string
}

// NewClient creates a client that discovers the Fastmail JMAP session at the
// standard endpoint. A nil HTTP client uses http.DefaultClient.
func NewClient(apiToken string, httpClient *http.Client) *Client {
	return newClient(apiToken, httpClient, defaultSessionURL)
}

func newClient(apiToken string, httpClient *http.Client, sessionURL string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	clientCopy := *httpClient
	if clientCopy.Timeout == 0 || clientCopy.Timeout > defaultRequestTimeout {
		clientCopy.Timeout = defaultRequestTimeout
	}
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		apiToken:   apiToken,
		httpClient: &clientCopy,
		sessionURL: sessionURL,
	}
}

// ListIdentityRecords reads all Masked Email records and, when available,
// standard JMAP submission identities. It performs one JMAP method request.
//
// The MaskedEmail extension defines only /get and /set, so records cannot be
// enumerated in chunks bounded by the session's maxObjectsInGet. Accounts with
// more records than that limit (Fastmail advertises 4096) fail with a typed
// *ObjectLimitError instead of an opaque method error.
func (c *Client) ListIdentityRecords(ctx context.Context) ([]Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	sessionURL, err := parseEndpoint(c.sessionURL)
	if err != nil {
		return nil, errors.New("invalid JMAP session endpoint")
	}
	session, err := c.fetchSession(ctx, sessionURL)
	if err != nil {
		return nil, err
	}
	if !hasCapability(session.Capabilities, CoreCapability) {
		return nil, &CapabilityError{Capability: CoreCapability}
	}
	if !hasCapability(session.Capabilities, MaskedEmailCapability) {
		return nil, &CapabilityError{Capability: MaskedEmailCapability}
	}

	maskedAccountID, err := selectCapabilityAccount(session, MaskedEmailCapability)
	if err != nil {
		return nil, err
	}

	using := []string{CoreCapability, MaskedEmailCapability}
	methods := []methodExpectation{{
		method:    maskedEmailGet,
		callID:    maskedCallID,
		accountID: maskedAccountID,
	}}
	if hasCapability(session.Capabilities, SubmissionCapability) {
		if submissionAccountID, selectErr := selectCapabilityAccount(session, SubmissionCapability); selectErr == nil {
			using = append(using, SubmissionCapability)
			methods = append(methods, methodExpectation{
				method:    "Identity/get",
				callID:    identityCallID,
				accountID: submissionAccountID,
			})
		}
	}

	apiURL, err := resolveAPIURL(sessionURL, session.APIURL)
	if err != nil {
		return nil, err
	}
	requestBody, err := buildJMAPRequest(using, methods)
	if err != nil {
		return nil, errors.New("encode JMAP identity request")
	}
	response, err := c.callMethods(ctx, apiURL, requestBody, methods)
	if err != nil {
		return nil, err
	}
	records, err := parseMethodResponses(apiURL, response, methods, coreObjectLimit(session.Capabilities))
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool {
		left := strings.ToLower(strings.TrimSpace(records[i].Identifier))
		right := strings.ToLower(strings.TrimSpace(records[j].Identifier))
		if left != right {
			return left < right
		}
		if records[i].Kind != records[j].Kind {
			return records[i].Kind < records[j].Kind
		}
		if records[i].State != records[j].State {
			return records[i].State < records[j].State
		}
		return records[i].Identifier < records[j].Identifier
	})
	return records, nil
}

func (c *Client) fetchSession(ctx context.Context, endpoint *url.URL) (sessionResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return sessionResponse{}, errors.New("create JMAP session request")
	}
	setRequestHeaders(request, c.apiToken, false)

	response, err := c.httpClient.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return sessionResponse{}, ctxErr
		}
		return sessionResponse{}, fmt.Errorf("request JMAP session from %s: transport failure", endpoint.Host)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return sessionResponse{}, fmt.Errorf(
			"JMAP session at %s returned HTTP status %d",
			endpoint.Host,
			response.StatusCode,
		)
	}

	var session sessionResponse
	if err := decodeJSON(response.Body, &session); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return sessionResponse{}, ctxErr
		}
		return sessionResponse{}, fmt.Errorf("decode JMAP session from %s", endpoint.Host)
	}
	return session, nil
}

func (c *Client) callMethods(
	ctx context.Context,
	endpoint *url.URL,
	body []byte,
	methods []methodExpectation,
) (jmapResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return jmapResponse{}, errors.New("create JMAP method request")
	}
	setRequestHeaders(request, c.apiToken, true)

	response, err := c.httpClient.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return jmapResponse{}, ctxErr
		}
		return jmapResponse{}, fmt.Errorf(
			"request %s from %s: transport failure",
			methodNames(methods),
			endpoint.Host,
		)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return jmapResponse{}, fmt.Errorf(
			"%s at %s returned HTTP status %d",
			methodNames(methods),
			endpoint.Host,
			response.StatusCode,
		)
	}

	var result jmapResponse
	if err := decodeJSON(response.Body, &result); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return jmapResponse{}, ctxErr
		}
		return jmapResponse{}, fmt.Errorf("decode %s response from %s", methodNames(methods), endpoint.Host)
	}
	return result, nil
}

func setRequestHeaders(request *http.Request, token string, hasBody bool) {
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	if hasBody {
		request.Header.Set("Content-Type", "application/json")
	}
}

func decodeJSON(reader io.Reader, target any) error {
	limited := io.LimitReader(reader, maxResponseBytes+1)
	decoder := json.NewDecoder(limited)
	return decoder.Decode(target)
}

func parseEndpoint(rawURL string) (*url.URL, error) {
	endpoint, err := url.Parse(rawURL)
	if err != nil || !endpoint.IsAbs() || endpoint.Host == "" || endpoint.User != nil {
		return nil, errors.New("invalid endpoint")
	}
	if endpoint.Scheme != "https" && endpoint.Scheme != "http" {
		return nil, errors.New("invalid endpoint")
	}
	return endpoint, nil
}

func resolveAPIURL(sessionURL *url.URL, rawAPIURL string) (*url.URL, error) {
	reference, err := url.Parse(rawAPIURL)
	if err != nil {
		return nil, fmt.Errorf("invalid JMAP API endpoint from %s", sessionURL.Host)
	}
	endpoint := sessionURL.ResolveReference(reference)
	if _, err := parseEndpoint(endpoint.String()); err != nil {
		return nil, fmt.Errorf("invalid JMAP API endpoint from %s", sessionURL.Host)
	}
	if !sameOriginOrSubdomain(sessionURL, endpoint) {
		return nil, fmt.Errorf("cross-origin JMAP API endpoint rejected for %s", sessionURL.Host)
	}
	return endpoint, nil
}

func sameOriginOrSubdomain(parent, candidate *url.URL) bool {
	if !strings.EqualFold(parent.Scheme, candidate.Scheme) ||
		effectivePort(parent) != effectivePort(candidate) {
		return false
	}
	parentHost := strings.ToLower(parent.Hostname())
	candidateHost := strings.ToLower(candidate.Hostname())
	return candidateHost == parentHost || strings.HasSuffix(candidateHost, "."+parentHost)
}

func effectivePort(endpoint *url.URL) string {
	if port := endpoint.Port(); port != "" {
		return port
	}
	if strings.EqualFold(endpoint.Scheme, "https") {
		return "443"
	}
	if strings.EqualFold(endpoint.Scheme, "http") {
		return "80"
	}
	return ""
}

func hasCapability(capabilities map[string]json.RawMessage, capability string) bool {
	_, ok := capabilities[capability]
	return ok
}

func selectCapabilityAccount(session sessionResponse, capability string) (string, error) {
	if accountID := session.PrimaryAccounts[capability]; accountID != "" {
		account, ok := session.Accounts[accountID]
		if !ok || !hasCapability(account.AccountCapabilities, capability) {
			return "", fmt.Errorf("primary account does not advertise JMAP capability %s", capability)
		}
		return accountID, nil
	}

	var matches []string
	for accountID, account := range session.Accounts {
		if hasCapability(account.AccountCapabilities, capability) {
			matches = append(matches, accountID)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no account advertises JMAP capability %s", capability)
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("ambiguous account selection for JMAP capability %s", capability)
	}
	return matches[0], nil
}

type methodExpectation struct {
	method    string
	callID    string
	accountID string
}

func buildJMAPRequest(using []string, methods []methodExpectation) ([]byte, error) {
	request := jmapRequest{Using: using, MethodCalls: make([][]json.RawMessage, 0, len(methods))}
	for _, method := range methods {
		name, err := json.Marshal(method.method)
		if err != nil {
			return nil, err
		}
		arguments, err := json.Marshal(struct {
			AccountID string `json:"accountId"`
		}{AccountID: method.accountID})
		if err != nil {
			return nil, err
		}
		callID, err := json.Marshal(method.callID)
		if err != nil {
			return nil, err
		}
		request.MethodCalls = append(request.MethodCalls, []json.RawMessage{name, arguments, callID})
	}
	return json.Marshal(request)
}

func coreObjectLimit(capabilities map[string]json.RawMessage) int64 {
	raw, ok := capabilities[CoreCapability]
	if !ok {
		return 0
	}
	var settings struct {
		MaxObjectsInGet int64 `json:"maxObjectsInGet"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return 0
	}
	return settings.MaxObjectsInGet
}

func methodErrorType(arguments json.RawMessage) string {
	var payload struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(arguments, &payload); err != nil {
		return ""
	}
	return payload.Type
}

func parseMethodResponses(
	endpoint *url.URL,
	response jmapResponse,
	methods []methodExpectation,
	maxObjectsInGet int64,
) ([]Record, error) {
	expected := make(map[string]methodExpectation, len(methods))
	for _, method := range methods {
		expected[method.callID] = method
	}
	seen := make(map[string]bool, len(methods))
	var records []Record
	for _, rawResponse := range response.MethodResponses {
		var tuple []json.RawMessage
		if err := json.Unmarshal(rawResponse, &tuple); err != nil || len(tuple) != 3 {
			return nil, fmt.Errorf("decode JMAP method response from %s", endpoint.Host)
		}
		var methodName, callID string
		if err := json.Unmarshal(tuple[0], &methodName); err != nil {
			return nil, fmt.Errorf("decode JMAP method name from %s", endpoint.Host)
		}
		if err := json.Unmarshal(tuple[2], &callID); err != nil {
			return nil, fmt.Errorf("decode JMAP call ID from %s", endpoint.Host)
		}
		method, ok := expected[callID]
		if !ok {
			return nil, fmt.Errorf("unexpected JMAP call ID from %s", endpoint.Host)
		}
		if seen[callID] {
			return nil, fmt.Errorf("duplicate JMAP call ID for %s from %s", method.method, endpoint.Host)
		}
		seen[callID] = true
		if methodName == "error" {
			if methodErrorType(tuple[1]) == "requestTooLarge" {
				return nil, &ObjectLimitError{Method: method.method, MaxObjectsInGet: maxObjectsInGet}
			}
			return nil, fmt.Errorf("%s at %s returned a JMAP method error", method.method, endpoint.Host)
		}
		if methodName != method.method {
			return nil, fmt.Errorf("unexpected JMAP method for %s from %s", method.method, endpoint.Host)
		}

		switch callID {
		case maskedCallID:
			var result maskedEmailGetResponse
			if err := json.Unmarshal(tuple[1], &result); err != nil {
				return nil, fmt.Errorf("decode %s response from %s", method.method, endpoint.Host)
			}
			if result.AccountID != method.accountID {
				return nil, fmt.Errorf("unexpected account in %s response from %s", method.method, endpoint.Host)
			}
			for _, item := range result.List {
				records = append(records, Record{
					Identifier: item.Email,
					State:      item.State,
					Kind:       "masked-email",
				})
			}
		case identityCallID:
			var result identityGetResponse
			if err := json.Unmarshal(tuple[1], &result); err != nil {
				return nil, fmt.Errorf("decode %s response from %s", method.method, endpoint.Host)
			}
			if result.AccountID != method.accountID {
				return nil, fmt.Errorf("unexpected account in %s response from %s", method.method, endpoint.Host)
			}
			for _, item := range result.List {
				records = append(records, Record{
					Identifier: item.Email,
					State:      "enabled",
					Kind:       "identity",
				})
			}
		}
	}

	for _, method := range methods {
		if !seen[method.callID] {
			return nil, fmt.Errorf("missing %s response from %s", method.method, endpoint.Host)
		}
	}
	return records, nil
}

func methodNames(methods []methodExpectation) string {
	names := make([]string, 0, len(methods))
	for _, method := range methods {
		names = append(names, method.method)
	}
	return strings.Join(names, " and ")
}
