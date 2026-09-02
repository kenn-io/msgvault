package peoplesweep

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	modelsDevURL              = "https://models.dev/api.json"
	modelsDevCatalogUserAgent = "OpenAI File Downloader, XaiImageApiFetch/1.0"
	modelsDevMaxBodyBytes     = 8 << 20
	modelsDevTotalTimeout     = 15 * time.Second
	maxModelsDevIDBytes       = 256
	maxModelsDevNameBytes     = 512
	maxModelsDevURLBytes      = 2048
)

var (
	ErrModelsDevUnavailable = errors.New("models.dev catalog is unavailable")
	ErrModelsDevTooLarge    = errors.New("models.dev catalog exceeds the size limit")
	ErrModelsDevInvalid     = errors.New("models.dev catalog is invalid")
	ErrModelsDevTimeout     = errors.New("models.dev catalog request timed out")

	modelsDevIDPattern          = regexp.MustCompile(`^[A-Za-z0-9@~][A-Za-z0-9._:/+@~-]{0,255}$`)
	modelsDevEnvironmentPattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,128}$`)
	modelsDevTemplatePattern    = regexp.MustCompile(`\$\{[A-Z][A-Z0-9_]{0,127}\}`)
)

// ProviderSuggestion is an advisory setup record. ProtocolCandidates is empty
// when the catalog shape is unsupported and contains every explicit choice
// when the catalog shape is ambiguous.
type ProviderSuggestion struct {
	ID                 string
	Name               string
	Endpoint           string
	EnvironmentNames   []string
	Models             []ModelSuggestion
	ProtocolCandidates []Protocol
}

// ModelSuggestion contains only onboarding hints. Prices are exact integer
// micro-USD per million tokens. Positive sub-micro fractions are rounded up so
// a catalog hint never understates its source price.
type ModelSuggestion struct {
	ID, Name                           string
	Reasoning, StructuredOutput        bool
	InputCostMicroUSDPerMillionTokens  *int64
	OutputCostMicroUSDPerMillionTokens *int64
}

// ModelsDevClient fetches the fixed public catalog only when its caller asks.
// It owns no config, credential, archive, store, or persistence dependency.
type ModelsDevClient struct {
	client *http.Client
	hooks  *modelsDevHooks
}

type modelsDevHooks struct {
	afterProvider func()
}

type modelsDevDialContext func(context.Context, string, string) (net.Conn, error)

// NewModelsDevClient constructs a fresh transport. The argument is retained
// for source compatibility but intentionally ignored so caller credentials,
// proxies, cookies, TLS identities, and later mutations cannot cross into the
// fixed public-catalog request.
func NewModelsDevClient(_ *http.Client) *ModelsDevClient {
	return newModelsDevClient(nil, nil, "")
}

func newModelsDevClientForTest(dial modelsDevDialContext, roots *x509.CertPool, serverName string) *ModelsDevClient {
	return newModelsDevClient(dial, roots, serverName)
}

func newModelsDevClient(dial modelsDevDialContext, roots *x509.CertPool, serverName string) *ModelsDevClient {
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	transport := &http.Transport{
		DialContext: dial, ForceAttemptHTTP2: true,
		MaxIdleConns: 2, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second,
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: serverName},
	}
	client := &http.Client{Transport: transport, Timeout: modelsDevTotalTimeout}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &ModelsDevClient{client: client}
}

// Fetch returns caller-owned discovery suggestions without caching or changing
// any system state.
func (c *ModelsDevClient) Fetch(ctx context.Context) ([]ProviderSuggestion, error) {
	if c == nil || c.client == nil || ctx == nil {
		return nil, ErrModelsDevUnavailable
	}
	requestCtx, cancel := context.WithTimeout(ctx, modelsDevTotalTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, modelsDevURL, nil)
	if err != nil {
		return nil, ErrModelsDevUnavailable
	}
	request.Header.Set("User-Agent", modelsDevCatalogUserAgent)
	response, err := c.client.Do(request) //nolint:bodyclose // Every successful response is drained and closed by disposeHTTPResponse below.
	if err != nil {
		if requestCtx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrModelsDevTimeout
		}
		return nil, ErrModelsDevUnavailable
	}
	defer disposeHTTPResponse(response.Body)
	if response.StatusCode != http.StatusOK {
		return nil, ErrModelsDevUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, modelsDevMaxBodyBytes+1))
	if requestCtx.Err() != nil {
		return nil, ErrModelsDevTimeout
	}
	if err != nil {
		return nil, ErrModelsDevUnavailable
	}
	if len(body) > modelsDevMaxBodyBytes {
		return nil, ErrModelsDevTooLarge
	}
	suggestions, err := parseModelsDevCatalog(requestCtx, body, c.hooks)
	if err != nil {
		if requestCtx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrModelsDevTimeout
		}
		return nil, ErrModelsDevInvalid
	}
	return suggestions, nil
}

func parseModelsDevCatalog(ctx context.Context, body []byte, hooks *modelsDevHooks) ([]ProviderSuggestion, error) {
	providers, err := decodeModelsDevObject(ctx, body)
	if err != nil {
		return nil, err
	}
	keys := sortedModelsDevKeys(providers)
	result := make([]ProviderSuggestion, 0, len(keys))
	seenIDs := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fields, fieldErr := decodeModelsDevObject(ctx, providers[key])
		if fieldErr != nil {
			return nil, fieldErr
		}
		id, fieldErr := requiredModelsDevString(fields, "id")
		if fieldErr != nil || id != key || !validModelsDevID(id) {
			return nil, ErrModelsDevInvalid
		}
		if _, duplicate := seenIDs[id]; duplicate {
			return nil, ErrModelsDevInvalid
		}
		seenIDs[id] = struct{}{}
		name, fieldErr := requiredModelsDevString(fields, "name")
		name = strings.TrimSpace(name)
		if fieldErr != nil || !validModelsDevText(name, maxModelsDevNameBytes) {
			return nil, ErrModelsDevInvalid
		}
		endpoint, fieldErr := optionalModelsDevString(fields, "api")
		if fieldErr != nil || !validModelsDevEndpoint(endpoint) {
			return nil, ErrModelsDevInvalid
		}
		environment, fieldErr := modelsDevEnvironment(fields["env"])
		if fieldErr != nil {
			return nil, fieldErr
		}
		models, fieldErr := modelsDevModels(ctx, fields["models"])
		if fieldErr != nil {
			return nil, fieldErr
		}
		shape, fieldErr := optionalModelsDevString(fields, "npm")
		if fieldErr != nil || !validModelsDevOptionalShape(shape) {
			return nil, ErrModelsDevInvalid
		}
		result = append(result, ProviderSuggestion{
			ID: id, Name: name, Endpoint: endpoint, EnvironmentNames: environment,
			Models: models, ProtocolCandidates: protocolsForModelsDevShape(shape),
		})
		if hooks != nil && hooks.afterProvider != nil {
			hooks.afterProvider()
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sort.Slice(result, func(left, right int) bool { return result[left].ID < result[right].ID })
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func modelsDevModels(ctx context.Context, raw json.RawMessage) ([]ModelSuggestion, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	models, err := decodeModelsDevObject(ctx, raw)
	if err != nil {
		return nil, err
	}
	keys := sortedModelsDevKeys(models)
	result := make([]ModelSuggestion, 0, len(keys))
	seenIDs := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fields, fieldErr := decodeModelsDevObject(ctx, models[key])
		if fieldErr != nil {
			return nil, fieldErr
		}
		id, fieldErr := requiredModelsDevString(fields, "id")
		if fieldErr != nil || id != key || !validModelsDevID(id) {
			return nil, ErrModelsDevInvalid
		}
		if _, duplicate := seenIDs[id]; duplicate {
			return nil, ErrModelsDevInvalid
		}
		seenIDs[id] = struct{}{}
		name, fieldErr := requiredModelsDevString(fields, "name")
		name = strings.TrimSpace(name)
		if fieldErr != nil || !validModelsDevText(name, maxModelsDevNameBytes) {
			return nil, ErrModelsDevInvalid
		}
		reasoning, fieldErr := optionalModelsDevBool(fields, "reasoning")
		if fieldErr != nil {
			return nil, fieldErr
		}
		structured, fieldErr := optionalModelsDevBool(fields, "structured_output")
		if fieldErr != nil {
			return nil, fieldErr
		}
		inputPrice, outputPrice, fieldErr := modelsDevPrices(ctx, fields["cost"])
		if fieldErr != nil {
			return nil, fieldErr
		}
		result = append(result, ModelSuggestion{
			ID: id, Name: name, Reasoning: reasoning, StructuredOutput: structured,
			InputCostMicroUSDPerMillionTokens:  inputPrice,
			OutputCostMicroUSDPerMillionTokens: outputPrice,
		})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].ID < result[right].ID })
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func modelsDevPrices(ctx context.Context, raw json.RawMessage) (*int64, *int64, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil, nil
	}
	fields, err := decodeModelsDevObject(ctx, raw)
	if err != nil {
		return nil, nil, err
	}
	input, err := modelsDevPrice(fields["input"])
	if err != nil {
		return nil, nil, err
	}
	output, err := modelsDevPrice(fields["output"])
	if err != nil {
		return nil, nil, err
	}
	return input, output, nil
}

func modelsDevPrice(raw json.RawMessage) (*int64, error) {
	if len(raw) == 0 {
		return nil, nil //nolint:nilnil // An omitted provider catalog price is an intentional optional value.
	}
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) == 0 || len(trimmed) > 64 || trimmed == "null" {
		return nil, ErrModelsDevInvalid
	}
	price, ok := new(big.Rat).SetString(trimmed)
	if !ok || price.Sign() < 0 {
		return nil, ErrModelsDevInvalid
	}
	price.Mul(price, big.NewRat(1_000_000, 1))
	value := new(big.Int)
	remainder := new(big.Int)
	value.QuoRem(price.Num(), price.Denom(), remainder)
	if remainder.Sign() != 0 {
		value.Add(value, big.NewInt(1))
	}
	if !value.IsInt64() {
		return nil, ErrModelsDevInvalid
	}
	converted := value.Int64()
	return &converted, nil
}

func modelsDevEnvironment(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var values []string
	if err := decodeModelsDevValue(raw, &values); err != nil {
		return nil, err
	}
	for _, value := range values {
		if !modelsDevEnvironmentPattern.MatchString(value) {
			return nil, ErrModelsDevInvalid
		}
	}
	slices.Sort(values)
	return slices.Compact(values), nil
}

func protocolsForModelsDevShape(shape string) []Protocol {
	switch shape {
	case "@ai-sdk/openai-compatible":
		return []Protocol{ProtocolOpenAIChat}
	case "@ai-sdk/openai":
		return []Protocol{ProtocolOpenAIChat, ProtocolOpenAIResponses}
	case "@ai-sdk/anthropic":
		return []Protocol{ProtocolAnthropicMessages}
	case "@ai-sdk/google":
		return []Protocol{ProtocolGoogleGenerateContent}
	default:
		return nil
	}
}

func decodeModelsDevObject(ctx context.Context, raw []byte) (map[string]json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, ErrModelsDevInvalid
	}
	result := make(map[string]json.RawMessage)
	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token, tokenErr := decoder.Token()
		key, ok := token.(string)
		if tokenErr != nil || !ok {
			return nil, ErrModelsDevInvalid
		}
		if _, duplicate := result[key]; duplicate {
			return nil, ErrModelsDevInvalid
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, ErrModelsDevInvalid
		}
		result[key] = append(json.RawMessage(nil), value...)
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return nil, ErrModelsDevInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrModelsDevInvalid
	}
	return result, nil
}

func decodeModelsDevValue(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return ErrModelsDevInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrModelsDevInvalid
	}
	return nil
}

func requiredModelsDevString(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok {
		return "", ErrModelsDevInvalid
	}
	var value string
	if err := decodeModelsDevValue(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func optionalModelsDevString(fields map[string]json.RawMessage, name string) (string, error) {
	raw, ok := fields[name]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var value string
	if err := decodeModelsDevValue(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}

func optionalModelsDevBool(fields map[string]json.RawMessage, name string) (bool, error) {
	raw, ok := fields[name]
	if !ok {
		return false, nil
	}
	var value bool
	if err := decodeModelsDevValue(raw, &value); err != nil {
		return false, err
	}
	return value, nil
}

func validModelsDevID(value string) bool {
	return len(value) <= maxModelsDevIDBytes && modelsDevIDPattern.MatchString(value)
}

func validModelsDevText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	format := unicode.Categories["Cf"]
	for _, char := range value {
		if unicode.IsControl(char) || (format != nil && unicode.Is(format, char)) {
			return false
		}
	}
	return true
}

func validModelsDevOptionalShape(value string) bool {
	return value == "" || validModelsDevText(value, maxModelsDevNameBytes)
}

func validModelsDevEndpoint(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > maxModelsDevURLBytes || !validModelsDevText(value, maxModelsDevURLBytes) {
		return false
	}
	expanded := modelsDevTemplatePattern.ReplaceAllString(value, "catalog-placeholder")
	if match := modelsDevTemplatePattern.FindStringIndex(value); match != nil && match[0] == 0 {
		suffix := value[match[1]:]
		if suffix != "" && !strings.HasPrefix(suffix, "/") {
			return false
		}
		expanded = "https://catalog-placeholder" +
			modelsDevTemplatePattern.ReplaceAllString(suffix, "catalog-placeholder")
	}
	if strings.ContainsAny(expanded, "${}") {
		return false
	}
	_, _, err := validateEndpoint(expanded)
	return err == nil
}

// independentlyTrustedEndpointHosts maps each HTTP protocol to the
// first-party API host this binary ships independently of any catalog.
// Catalog suggestions may pair a credential only with these hosts; every
// other catalog-supplied endpoint requires an explicitly supplied endpoint
// so a compromised catalog can never redirect a credential to itself.
var independentlyTrustedEndpointHosts = map[Protocol]string{
	ProtocolOpenAIChat:            "api.openai.com",
	ProtocolOpenAIResponses:       "api.openai.com",
	ProtocolAnthropicMessages:     "api.anthropic.com",
	ProtocolGoogleGenerateContent: "generativelanguage.googleapis.com",
}

// IndependentlyTrustedEndpoint reports whether endpoint is an HTTPS endpoint
// on the first-party API host compiled into this binary for protocol. Trust
// never depends on catalog input: paths and the default port may vary because
// a credential is only ever presented to the first-party TLS endpoint. It
// returns false for every other host, scheme, port, or protocol.
func IndependentlyTrustedEndpoint(protocol Protocol, endpoint string) bool {
	host, trusted := independentlyTrustedEndpointHosts[protocol]
	if !trusted || endpoint == "" {
		return false
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return false
	}
	if !strings.EqualFold(parsed.Hostname(), host) {
		return false
	}
	port := parsed.Port()
	return port == "" || port == "443"
}

func sortedModelsDevKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
