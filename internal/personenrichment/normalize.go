package personenrichment

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/textimport"
	"golang.org/x/net/idna"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	EmailNormalizationV1            = "email-v1"
	PhoneNormalizationV1            = "phone-v1"
	URLNormalizationV1              = "public-url-v1"
	CompositeNormalizationV1        = "name-company-v1"
	ProviderPersonIDNormalizationV1 = "provider-person-id-v1"
)

type NormalizedIdentifier struct {
	Class                IdentifierClass
	NormalizationVersion string
	Value                string
}

type NormalizedSuppressionIdentifier struct {
	Class                SuppressionIdentifierClass
	NormalizationVersion string
	Value                string
}

func NormalizeIdentifier(class IdentifierClass, value string) (NormalizedIdentifier, error) {
	normalized := NormalizedIdentifier{Class: class}
	switch class {
	case IdentifierEmail:
		normalized.NormalizationVersion = EmailNormalizationV1
		normalized.Value = strings.ToLower(strings.TrimSpace(value))
	case IdentifierPhone:
		normalized.NormalizationVersion = PhoneNormalizationV1
		phone, err := textimport.NormalizePhone(value)
		if err != nil {
			return NormalizedIdentifier{}, fmt.Errorf("normalize phone: %w", err)
		}
		normalized.Value = phone
	case IdentifierPublicProfileURL:
		normalized.NormalizationVersion = URLNormalizationV1
		canonical, err := CanonicalPublicURL(value)
		if err != nil {
			return NormalizedIdentifier{}, err
		}
		normalized.Value = canonical
	case IdentifierName, IdentifierCurrentCompany:
		normalized.NormalizationVersion = CompositeNormalizationV1
		normalized.Value = normalizeCompositePart(value)
	default:
		return NormalizedIdentifier{}, fmt.Errorf("unsupported identifier class %q", class)
	}
	if normalized.Value == "" {
		return NormalizedIdentifier{}, fmt.Errorf("%s identifier must be non-empty", class)
	}
	return normalized, nil
}

func NormalizeSuppressionIdentifier(
	class SuppressionIdentifierClass,
	values []string,
) (NormalizedSuppressionIdentifier, error) {
	if class == SuppressionNameCompany {
		if len(values) != 2 {
			return NormalizedSuppressionIdentifier{}, errors.New("name_company requires exactly name and company")
		}
		name := normalizeCompositePart(values[0])
		company := normalizeCompositePart(values[1])
		if name == "" || company == "" {
			return NormalizedSuppressionIdentifier{}, errors.New("name_company requires non-empty name and company")
		}
		return NormalizedSuppressionIdentifier{
			Class: class, NormalizationVersion: CompositeNormalizationV1,
			Value: lengthDelimited(name) + lengthDelimited(company),
		}, nil
	}
	if len(values) != 1 {
		return NormalizedSuppressionIdentifier{}, fmt.Errorf("%s requires exactly one value", class)
	}
	identifierClass, version, err := suppressionScalarPolicy(class)
	if err != nil {
		return NormalizedSuppressionIdentifier{}, err
	}
	if class == SuppressionProviderPersonID {
		value := strings.TrimSpace(values[0])
		if value == "" {
			return NormalizedSuppressionIdentifier{}, errors.New("provider_person_id must be non-empty")
		}
		return NormalizedSuppressionIdentifier{
			Class: class, NormalizationVersion: ProviderPersonIDNormalizationV1, Value: value,
		}, nil
	}
	normalized, err := NormalizeIdentifier(identifierClass, values[0])
	if err != nil {
		return NormalizedSuppressionIdentifier{}, err
	}
	return NormalizedSuppressionIdentifier{
		Class: class, NormalizationVersion: version, Value: normalized.Value,
	}, nil
}

func suppressionScalarPolicy(class SuppressionIdentifierClass) (IdentifierClass, string, error) {
	switch class {
	case SuppressionEmail:
		return IdentifierEmail, EmailNormalizationV1, nil
	case SuppressionPhone:
		return IdentifierPhone, PhoneNormalizationV1, nil
	case SuppressionPublicProfileURL:
		return IdentifierPublicProfileURL, URLNormalizationV1, nil
	case SuppressionProviderPersonID:
		return "", ProviderPersonIDNormalizationV1, nil
	default:
		return "", "", fmt.Errorf("unsupported suppression identifier class %q", class)
	}
}

func CanonicalPublicURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", fmt.Errorf("parse public profile URL: %w", err)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("public profile URL must use HTTP or HTTPS")
	}
	if parsed.User != nil {
		return "", errors.New("public profile URL must not contain userinfo")
	}
	if parsed.Opaque != "" {
		return "", errors.New("public profile URL must not be opaque")
	}
	hostname := parsed.Hostname()
	if hostname == "" {
		return "", errors.New("public profile URL requires a host")
	}
	canonicalHost, err := canonicalURLHost(hostname)
	if err != nil {
		return "", err
	}
	port := parsed.Port()
	if port != "" {
		// Numeric ports are canonicalized without leading zeroes so
		// equivalent URLs hash to one suppression identity.
		if numeric, err := strconv.Atoi(port); err == nil {
			port = strconv.Itoa(numeric)
		}
		if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
			port = ""
		}
	}
	if port != "" {
		parsed.Host = net.JoinHostPort(canonicalHost, port)
	} else if strings.Contains(canonicalHost, ":") {
		parsed.Host = "[" + canonicalHost + "]"
	} else {
		parsed.Host = canonicalHost
	}

	cleanedEscapedPath := path.Clean(parsed.EscapedPath())
	if cleanedEscapedPath == "." || cleanedEscapedPath == "" {
		cleanedEscapedPath = "/"
	}
	if !strings.HasPrefix(cleanedEscapedPath, "/") {
		cleanedEscapedPath = "/" + cleanedEscapedPath
	}
	cleanedEscapedPath = uppercasePercentEscapes(cleanedEscapedPath)
	cleanedPath, err := url.PathUnescape(cleanedEscapedPath)
	if err != nil {
		return "", fmt.Errorf("decode cleaned public profile URL path: %w", err)
	}
	parsed.Path = cleanedPath
	parsed.RawPath = cleanedEscapedPath
	parsed.Fragment = ""
	parsed.RawFragment = ""

	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", fmt.Errorf("parse public profile URL query: %w", err)
	}
	for key, queryValues := range query {
		if isTrackingQueryKey(key) {
			delete(query, key)
			continue
		}
		slices.Sort(queryValues)
		query[key] = queryValues
	}
	parsed.RawQuery = query.Encode()
	parsed.ForceQuery = false
	return parsed.String(), nil
}

func uppercasePercentEscapes(value string) string {
	var normalized strings.Builder
	normalized.Grow(len(value))
	for i := 0; i < len(value); i++ {
		normalized.WriteByte(value[i])
		if value[i] != '%' || i+2 >= len(value) {
			continue
		}
		normalized.WriteString(strings.ToUpper(value[i+1 : i+3]))
		i += 2
	}
	return normalized.String()
}

func canonicalURLHost(host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return strings.ToLower(ip.String()), nil
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		return "", fmt.Errorf("normalize public profile URL host: %w", err)
	}
	if ascii == "" {
		return "", errors.New("public profile URL requires a host")
	}
	return strings.ToLower(ascii), nil
}

func isTrackingQueryKey(key string) bool {
	lower := strings.ToLower(key)
	if strings.HasPrefix(lower, "utm_") {
		return true
	}
	switch lower {
	case "fbclid", "gclid", "dclid", "msclkid", "mc_cid", "mc_eid":
		return true
	default:
		return false
	}
}

func normalizeCompositePart(value string) string {
	return strings.Join(strings.Fields(cases.Fold().String(norm.NFKC.String(value))), " ")
}

func lengthDelimited(value string) string {
	return strconv.Itoa(len([]byte(value))) + ":" + value
}

func ConfidenceScore01(value float64) (int, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		return 0, fmt.Errorf("confidence %v must be finite and in [0,1]", value)
	}
	return int(math.Floor(value*1000 + 0.5)), nil
}
