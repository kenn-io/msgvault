package personenrichment_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
)

func TestNormalizeIdentifierUsesVersionedCanonicalForms(t *testing.T) {
	tests := []struct {
		name    string
		class   personenrichment.IdentifierClass
		value   string
		version string
		want    string
	}{
		{name: "email", class: personenrichment.IdentifierEmail, value: "  USER@Example.COM  ", version: personenrichment.EmailNormalizationV1, want: "user@example.com"},
		{name: "ten digit phone", class: personenrichment.IdentifierPhone, value: "(415) 555-0123", version: personenrichment.PhoneNormalizationV1, want: "+14155550123"},
		{name: "public URL", class: personenrichment.IdentifierPublicProfileURL, value: "HTTPS://BÜCHER.Example:443/a/../profile/?utm_source=test&b=2&a=1#bio", version: personenrichment.URLNormalizationV1, want: "https://xn--bcher-kva.example/profile?a=1&b=2"},
		{name: "name NFKC folding", class: personenrichment.IdentifierName, value: "  Ａlice\tEXAMPLE  ", version: personenrichment.CompositeNormalizationV1, want: "alice example"},
		{name: "company NFKC folding", class: personenrichment.IdentifierCurrentCompany, value: "  ＡCME\nLabs  ", version: personenrichment.CompositeNormalizationV1, want: "acme labs"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checks := assert.New(t)
			got, err := personenrichment.NormalizeIdentifier(test.class, test.value)
			require.NoError(t, err)
			checks.Equal(test.class, got.Class)
			checks.Equal(test.version, got.NormalizationVersion)
			checks.Equal(test.want, got.Value)
		})
	}
}

func TestNormalizeIdentifierRejectsEmptyInvalidAndUnknownValues(t *testing.T) {
	tests := []struct {
		name  string
		class personenrichment.IdentifierClass
		value string
	}{
		{name: "empty email", class: personenrichment.IdentifierEmail, value: " \t "},
		{name: "invalid phone", class: personenrichment.IdentifierPhone, value: "extension five"},
		{name: "invalid URL", class: personenrichment.IdentifierPublicProfileURL, value: "mailto:user@example.com"},
		{name: "empty name", class: personenrichment.IdentifierName, value: "\n"},
		{name: "unknown class", class: personenrichment.IdentifierClass("chat"), value: "CHAT_SENTINEL"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := personenrichment.NormalizeIdentifier(test.class, test.value)
			require.Error(t, err)
		})
	}
}

func TestCanonicalPublicURLNormalizesAuthorityPathAndTrackingQueries(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "HTTPS IDNA default port fragment and query", value: " HTTPS://BÜCHER.Example:443/a/../profile/?utm_source=test&b=2&a=1#bio ", want: "https://xn--bcher-kva.example/profile?a=1&b=2"},
		{name: "HTTP default port and root slash", value: "http://EXAMPLE.com:80", want: "http://example.com/"},
		{name: "nondefault port", value: "https://Example.COM:8443/team//alice/", want: "https://example.com:8443/team/alice"},
		{name: "tracking keys case insensitive", value: "https://example.com/p?GCLID=one&fbclid=two&keep=yes&UTM_campaign=three", want: "https://example.com/p?keep=yes"},
		{name: "HTTP default port with leading zeroes", value: "http://EXAMPLE.com:080", want: "http://example.com/"},
		{name: "HTTPS default port with leading zeroes", value: "https://Example.COM:0443/team//alice/", want: "https://example.com/team/alice"},
		{name: "nondefault port with leading zeroes canonicalized", value: "https://example.com:08443/x", want: "https://example.com:8443/x"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := personenrichment.CanonicalPublicURL(test.value)
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestCanonicalPublicURLPreservesEscapedReservedPathCharacters(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	escaped, err := personenrichment.CanonicalPublicURL(
		"https://example.com/root//a%2fb/./profile/",
	)
	requirements.NoError(err)
	checks.Equal("https://example.com/root/a%2Fb/profile", escaped)

	separator, err := personenrichment.CanonicalPublicURL(
		"https://example.com/root/a/b/profile",
	)
	requirements.NoError(err)
	checks.Equal("https://example.com/root/a/b/profile", separator)
	checks.NotEqual(escaped, separator)
}

func TestCanonicalPublicURLRejectsNonPublicURLShapes(t *testing.T) {
	for _, value := range []string{
		"ftp://example.com/profile",
		"https://user:secret@example.com/profile",
		"https:///missing-host",
		"https://example.com:invalid/profile",
	} {
		t.Run(value, func(t *testing.T) {
			_, err := personenrichment.CanonicalPublicURL(value)
			require.Error(t, err)
		})
	}
}

func TestNormalizeSuppressionIdentifierUsesExactVersionedSemantics(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	email, err := personenrichment.NormalizeSuppressionIdentifier(
		personenrichment.SuppressionEmail, []string{" User@Example.COM "},
	)
	requirements.NoError(err)
	checks.Equal(personenrichment.NormalizedSuppressionIdentifier{
		Class: personenrichment.SuppressionEmail, NormalizationVersion: personenrichment.EmailNormalizationV1,
		Value: "user@example.com",
	}, email)

	providerID, err := personenrichment.NormalizeSuppressionIdentifier(
		personenrichment.SuppressionProviderPersonID, []string{"  AbC/Case-Sensitive:id  "},
	)
	requirements.NoError(err)
	checks.Equal(personenrichment.NormalizedSuppressionIdentifier{
		Class: personenrichment.SuppressionProviderPersonID, NormalizationVersion: personenrichment.ProviderPersonIDNormalizationV1,
		Value: "AbC/Case-Sensitive:id",
	}, providerID)

	composite, err := personenrichment.NormalizeSuppressionIdentifier(
		personenrichment.SuppressionNameCompany, []string{" ＡLICE  Example ", " Acme\tLABS "},
	)
	requirements.NoError(err)
	checks.Equal(personenrichment.NormalizedSuppressionIdentifier{
		Class: personenrichment.SuppressionNameCompany, NormalizationVersion: personenrichment.CompositeNormalizationV1,
		Value: "13:alice example9:acme labs",
	}, composite)
}

func TestNormalizeSuppressionIdentifierRejectsWrongArityAndNameAlone(t *testing.T) {
	tests := []struct {
		name   string
		class  personenrichment.SuppressionIdentifierClass
		values []string
	}{
		{name: "name alone is not a class", class: personenrichment.SuppressionIdentifierClass("name"), values: []string{"Alice Example"}},
		{name: "missing company", class: personenrichment.SuppressionNameCompany, values: []string{"Alice Example"}},
		{name: "blank company", class: personenrichment.SuppressionNameCompany, values: []string{"Alice Example", " "}},
		{name: "scalar has two values", class: personenrichment.SuppressionEmail, values: []string{"a@example.com", "b@example.com"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := personenrichment.NormalizeSuppressionIdentifier(test.class, test.values)
			require.Error(t, err)
		})
	}
}

func TestConfidenceScore01RoundsExactlyAndRejectsNonFiniteOrOutOfRange(t *testing.T) {
	tests := []struct {
		value float64
		want  int
	}{
		{value: 0, want: 0},
		{value: 0.899, want: 899},
		{value: 0.90, want: 900},
		{value: 0.9005, want: 901},
		{value: 1, want: 1000},
	}
	for _, test := range tests {
		got, err := personenrichment.ConfidenceScore01(test.value)
		require.NoError(t, err)
		assert.Equal(t, test.want, got)
	}

	for _, value := range []float64{-0.001, 1.001, math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := personenrichment.ConfidenceScore01(value)
		require.Error(t, err)
	}
}
