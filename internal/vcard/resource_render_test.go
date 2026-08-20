package vcard

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forcedRender re-renders a vCard 4 envelope whose stored body already is
// vCard 4. RenderView otherwise returns StoredBody byte-for-byte.
func forcedRender(t *testing.T, envelope ResourceEnvelope) []byte {
	t.Helper()
	envelope.RenderMetadata.RenderRequired = true
	body, err := envelope.RenderView(Version40)
	require.NoError(t, err)
	return body
}

func TestPrepareCanonicalRenderKeepsUntouchedV4PropertiesVerbatim(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice Example\r\n" +
		"EMAIL;VALUE=text;TYPE=home:alice@example.com\r\n" +
		"TEL;VALUE=uri:tel:+12025550123\r\n" +
		"X-FOO;TYPE=pref:x\r\n" +
		"GEO:37;-122\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	fullName := propertyByNameAndGroup(envelope.PropertyTree, "FN", "")
	replacement, err := NewProperty("", "FN", "Alice Example")
	require.NoError(err)
	edited, err := envelope.MergeProperties([]PropertyEdit{{
		Identity: fullName.Identity, Property: replacement,
	}})
	require.NoError(err)

	canonical, err := edited.PrepareCanonicalRender()
	require.NoError(err)
	assert.Equal(string(raw), string(canonical.StoredBody))
	assert.Equal(envelope.ETag, canonical.ETag)
	assert.Equal(envelope.RenderMetadata.Revision, canonical.RenderMetadata.Revision)
	assert.False(canonical.RenderMetadata.RenderRequired)
}

func TestCanonicalRenderIsIdempotentAcrossFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*.vcf"))
	require.NoError(t, err)
	require.NotEmpty(t, paths)
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			raw, err := os.ReadFile(path)
			require.NoError(err)
			envelope, err := ParseResourceEnvelope(raw)
			require.NoError(err)
			envelope.RenderMetadata.RenderRequired = true
			first, err := envelope.PrepareCanonicalRender()
			require.NoError(err)

			first.RenderMetadata.RenderRequired = true
			second, err := first.PrepareCanonicalRender()
			require.NoError(err)
			assert.Equal(string(first.StoredBody), string(second.StoredBody))
			assert.Equal(first.ETag, second.ETag)
			assert.Equal(first.RenderMetadata.Revision, second.RenderMetadata.Revision)
			assert.Equal(first.PropertyTree, second.PropertyTree)

			reparsed, err := ParseResourceEnvelope(second.StoredBody)
			require.NoError(err)
			assertSameProperties(t, second.PropertyTree, reparsed.PropertyTree)
		})
	}
}

func TestV4RenderRoundTripsFixtureTrees(t *testing.T) {
	fixtures := []string{"v4-registry-smoke.vcf", "v4-rfc9554.vcf", "unknown-extensions.vcf"}
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			raw, err := os.ReadFile(filepath.Join("testdata", name))
			require.NoError(err)
			envelope, err := ParseResourceEnvelope(raw)
			require.NoError(err)
			rendered := forcedRender(t, envelope)
			reparsed, err := ParseResourceEnvelope(rendered)
			require.NoError(err)
			assertSameProperties(t, envelope.PropertyTree, reparsed.PropertyTree)
			assert.Equal(t, envelope.PropertyTree, reparsed.PropertyTree)
		})
	}
}

func TestV3ViewKeepsURIShapedTextTelephoneVerbatim(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	envelope, err := ParseResourceEnvelope([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\n" +
		"FN:Alice\r\nTEL;VALUE=text:tel:+12025550123\r\nEND:VCARD\r\n"))
	require.NoError(err)
	v3, err := envelope.RenderView(Version30)
	require.NoError(err)
	// The value is declared text: that it happens to look like a tel URI does
	// not make it one, so no scheme stripping and no unescaping.
	assert.Contains(string(v3), "TEL:tel:+12025550123\r\n")
	reparsed, err := ParseResourceEnvelope(v3)
	require.NoError(err)
	tel := propertyByNameAndGroup(reparsed.PropertyTree, "TEL", "")
	assert.Equal("tel:+12025550123", tel.Property.RawValue)
}

func TestV4RenderKeepsFreeFormTelephoneAsText(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Alice\r\n" +
		"TEL;VALUE=text:call the front desk\r\n" +
		"TEL:+49/30/123\r\n" +
		"TEL:+1 (202) 555-0123\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	v4, err := envelope.RenderView(Version40)
	require.NoError(err)
	assert.Contains(string(v4), "TEL;VALUE=text:call the front desk\r\n")
	assert.Contains(string(v4), "TEL;VALUE=text:+49/30/123\r\n")
	assert.Contains(string(v4), "TEL:tel:+1(202)555-0123\r\n")

	canonical, err := envelope.PrepareCanonicalRender()
	require.NoError(err)
	assert.Equal(v4, canonical.StoredBody)
	v3, err := canonical.RenderView(Version30)
	require.NoError(err)
	assert.Contains(string(v3), "TEL:call the front desk\r\n")
	assert.Contains(string(v3), "TEL:+49/30/123\r\n")
	assert.NotContains(string(v3), "VALUE=text")
}

func TestV3ViewDegradesUnrepresentableOccurrencesInsteadOfFailing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"BDAY;VALUE=text:circa 1970\r\n" +
		"REV;VALUE=text:long ago\r\n" +
		"X-FUTURE;X-CARET=A^^B^'C;X-KEEP=yes:opaque\r\n" +
		"X-LIST;X-MIXED=plain,A^'B:v\r\n" +
		"GEO:geo:37.386013,-122.082932;u=35\r\n" +
		"KIND:individual\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)

	v3, err := envelope.RenderView(Version30)
	require.NoError(err)
	body := string(v3)
	assert.NotContains(body, "BDAY")
	assert.NotContains(body, "REV")
	assert.NotContains(body, "GEO")
	assert.NotContains(body, "KIND")
	assert.Contains(body, "X-FUTURE;X-KEEP=yes:opaque\r\n")
	assert.Contains(body, "X-LIST;X-MIXED=plain:v\r\n")

	v4, err := envelope.RenderView(Version40)
	require.NoError(err)
	assert.Equal(raw, v4)
}

func TestV3ViewSpellsInlineMediaAsLegacyBinary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"PHOTO:data:image/jpeg;base64,/9j/4AAQSkZJRgABAQA=\r\n" +
		"LOGO;TYPE=work:data:image/png;base64,AA==\r\n" +
		"KEY:data:application/pgp-keys;base64,AAEC\r\n" +
		"SOUND:data:audio/basic;base64,AA==\r\n" +
		"PHOTO;MEDIATYPE=image/gif:https://example.com/a.gif\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)

	v3, err := envelope.RenderView(Version30)
	require.NoError(err)
	body := string(v3)
	assert.Contains(body, "PHOTO;ENCODING=b;TYPE=JPEG:/9j/4AAQSkZJRgABAQA=\r\n")
	assert.Contains(body, "LOGO;TYPE=work,PNG;ENCODING=b:AA==\r\n")
	assert.Contains(body, "KEY;ENCODING=b;TYPE=PGP:AAEC\r\n")
	assert.Contains(body, "SOUND;ENCODING=b;TYPE=BASIC:AA==\r\n")
	assert.Contains(body, "PHOTO;TYPE=GIF;VALUE=uri:https://example.com/a.gif\r\n")
	assert.NotContains(body, "data:")

	// The compatibility view converts back to the canonical data URIs.
	reparsed, err := ParseResourceEnvelope(v3)
	require.NoError(err)
	v4, err := reparsed.RenderView(Version40)
	require.NoError(err)
	assert.Contains(string(v4), "PHOTO:data:image/jpeg;base64,/9j/4AAQSkZJRgABAQA=\r\n")
	assert.Contains(string(v4), "LOGO;TYPE=work:data:image/png;base64,AA==\r\n")
	assert.Contains(string(v4), "KEY:data:application/pgp-keys;base64,AAEC\r\n")
}

func TestLegacyMediaTokensResolveFromAFixedTable(t *testing.T) {
	tests := []struct {
		token string
		want  string
	}{
		{token: "BMP", want: "PHOTO:data:image/bmp;base64,AAEC\r\n"},
		{token: "tiff", want: "PHOTO:data:image/tiff;base64,AAEC\r\n"},
		{token: "JPEG", want: "PHOTO:data:image/jpeg;base64,AAEC\r\n"},
		{token: "FOO", want: "PHOTO;TYPE=FOO:data:application/octet-stream;base64,AAEC\r\n"},
	}
	for _, test := range tests {
		t.Run(test.token, func(t *testing.T) {
			require := require.New(t)
			raw := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Alice\r\n" +
				"PHOTO;ENCODING=b;TYPE=" + test.token + ":AAEC\r\nEND:VCARD\r\n")
			envelope, err := ParseResourceEnvelope(raw)
			require.NoError(err)
			v4, err := envelope.RenderView(Version40)
			require.NoError(err)
			assert.Contains(t, string(v4), test.want)
		})
	}
	assert.Empty(t, mediaTypeFromToken("html"), "host mime tables must not resolve tokens")
}

func TestV21BackslashesAreLiteralText(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:2.1\r\nFN:Alice\r\n" +
		"NOTE;ENCODING=QUOTED-PRINTABLE:C:\\Users\\name\r\n" +
		"X-PATH:D:\\Nested\\;dir\r\n" +
		"N:Doe\\;Jr;John;;;\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	for _, target := range []Version{Version30, Version40} {
		view, err := envelope.RenderView(target)
		require.NoError(err, "%s", target)
		body := string(view)
		assert.Contains(body, "NOTE:C:\\\\Users\\\\name\r\n", "%s", target)
		assert.Contains(body, "X-PATH:D:\\\\Nested\\;dir\r\n", "%s", target)
		assert.Contains(body, "N:Doe\\;Jr;John;;;\r\n", "%s", target)
	}
	canonical, err := envelope.PrepareCanonicalRender()
	require.NoError(err)
	note := propertyByNameAndGroup(canonical.PropertyTree, "NOTE", "")
	text, err := UnescapeText(note.Property.RawValue)
	require.NoError(err)
	assert.Equal("C:\\Users\\name", text)
}

func TestMergeIntoV21EnvelopeKeepsProjectedEscapes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:2.1\r\nFN:Alice\r\n" +
		"X-PATH:D:\\Nested\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	// A projected value arrives already in vCard 4 wire form: "\n" is a line
	// break and "\," a comma. It must not be read a second time as legacy 2.1
	// text, which would double its backslashes.
	note, err := NewProperty("", "NOTE", "line one\\nline two\\, with comma")
	require.NoError(err)
	fn := propertyByNameAndGroup(envelope.PropertyTree, "FN", "")
	replacement, err := NewProperty("", "FN", "Alice\\, Example")
	require.NoError(err)
	merged, err := envelope.MergeProperties([]PropertyEdit{
		{Property: note},
		{Identity: fn.Identity, Property: replacement},
	})
	require.NoError(err)
	canonical, err := merged.PrepareCanonicalRender()
	require.NoError(err)
	body := string(canonical.StoredBody)
	assert.Contains(body, "NOTE:line one\\nline two\\, with comma\r\n")
	assert.Contains(body, "FN:Alice\\, Example\r\n")
	assert.Contains(body, "X-PATH:D:\\\\Nested\r\n",
		"the imported 2.1 value still has its literal backslash escaped once")
	text, err := UnescapeText(propertyByNameAndGroup(
		canonical.PropertyTree, "NOTE", "").Property.RawValue)
	require.NoError(err)
	assert.Equal("line one\nline two, with comma", text)
	assert.Equal(Version40, canonical.RenderMetadata.StoredVersion)
}

func TestLegacyUndeclaredCharsetFallsBackToLatin1(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	envelope, err := ParseResourceEnvelope([]byte("BEGIN:VCARD\r\nVERSION:2.1\r\n" +
		"FN:Alice\r\nX-MOTTO;QUOTED-PRINTABLE:Sant=E9\r\nEND:VCARD\r\n"))
	require.NoError(err)
	canonical, err := envelope.PrepareCanonicalRender()
	require.NoError(err, "undeclared non-UTF-8 text reads as ISO-8859-1")
	assert.Contains(string(canonical.StoredBody), "X-MOTTO:Santé\r\n")

	declared, err := ParseResourceEnvelope([]byte("BEGIN:VCARD\r\nVERSION:2.1\r\n" +
		"FN:Alice\r\nX-MOTTO;CHARSET=UTF-8;QUOTED-PRINTABLE:Sant=E9\r\nEND:VCARD\r\n"))
	require.NoError(err)
	_, err = declared.PrepareCanonicalRender()
	require.Error(err, "a declared charset the bytes do not satisfy is still refused")
	assert.Contains(err.Error(), "not valid UTF-8")
}

func TestLegacyRawLatin1ValuesDecodeThroughTheFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// A bare Latin-1 byte with no CHARSET declaration, as vCard 2.1 producers
	// wrote in practice. The line must survive parsing so the render-time
	// Latin-1 fallback can decode it.
	envelope, err := ParseResourceEnvelope([]byte("BEGIN:VCARD\r\nVERSION:2.1\r\n" +
		"FN:Alice\r\nNOTE:Caf\xe9\r\nEND:VCARD\r\n"))
	require.NoError(err, "raw Latin-1 in a 2.1 card parses")
	canonical, err := envelope.PrepareCanonicalRender()
	require.NoError(err)
	assert.Contains(string(canonical.StoredBody), "NOTE:Café\r\n")

	// The same bytes in 3.0 and 4.0 cards stay rejected: those versions are
	// UTF-8 unless a CHARSET parameter says otherwise.
	for _, version := range []string{"3.0", "4.0"} {
		_, err := ParseResourceEnvelope([]byte("BEGIN:VCARD\r\nVERSION:" + version +
			"\r\nFN:Alice\r\nNOTE:Caf\xe9\r\nEND:VCARD\r\n"))
		require.Error(err, "%s", version)
		assert.Contains(err.Error(), "not valid UTF-8", "%s", version)
	}
}

func TestV3ViewMapsV4ValueTypesAndEscapesTelURIParameters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"X-D;VALUE=date-and-or-time:20200101\r\n" +
		"X-T;VALUE=timestamp:20200101T000000Z\r\n" +
		"X-L;VALUE=language-tag:en\r\n" +
		"X-I;VALUE=integer:7\r\n" +
		"TEL:tel:+12025550123;ext=12\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	v3, err := envelope.RenderView(Version30)
	require.NoError(err)
	body := string(v3)
	assert.Contains(body, "X-D:20200101\r\n")
	assert.Contains(body, "X-T;VALUE=date-time:20200101T000000Z\r\n")
	assert.Contains(body, "X-L:en\r\n")
	assert.Contains(body, "X-I;VALUE=integer:7\r\n")
	assert.Contains(body, "TEL:+12025550123\\;ext=12\r\n")

	reparsed, err := ParseResourceEnvelope(v3)
	require.NoError(err)
	tel := propertyByNameAndGroup(reparsed.PropertyTree, "TEL", "")
	number, err := UnescapeText(tel.Property.RawValue)
	require.NoError(err)
	assert.Equal("+12025550123;ext=12", number)
}

func TestRenderViewRejectsUnsupportedVersion(t *testing.T) {
	require := require.New(t)
	envelope, err := ParseResourceEnvelope(
		[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n"),
	)
	require.NoError(err)
	_, err = envelope.RenderView(Version("5.0"))
	require.ErrorContains(err, `unsupported vCard render version "5.0"`)
	_, err = envelope.RenderView(Version21)
	require.ErrorContains(err, "read-only")
}

func TestParseResourceEnvelopeAcceptsLFOnlyAndBOMBodies(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	crlf := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice Example\r\n" +
		"NOTE:line one\r\nEND:VCARD\r\n")
	want, err := ParseResourceEnvelope(crlf)
	require.NoError(err)

	lfOnly := bytes.ReplaceAll(crlf, []byte("\r\n"), []byte("\n"))
	fromLF, err := ParseResourceEnvelope(lfOnly)
	require.NoError(err)
	assert.Equal(lfOnly, fromLF.StoredBody)
	assert.Equal(want.PropertyTree, fromLF.PropertyTree)
	rendered := forcedRender(t, fromLF)
	assert.Equal(string(crlf), string(rendered))

	withBOM := append([]byte{0xef, 0xbb, 0xbf}, crlf...)
	fromBOM, err := ParseResourceEnvelope(withBOM)
	require.NoError(err)
	assert.Equal(withBOM, fromBOM.StoredBody)
	assert.Equal(want.PropertyTree, fromBOM.PropertyTree)
	canonical, err := fromBOM.PrepareCanonicalRender()
	require.NoError(err)
	assert.Equal(withBOM, canonical.StoredBody, "an unchanged body is never re-rendered")
	assert.Equal(string(crlf), string(forcedRender(t, fromBOM)))
}

func TestPrepareCanonicalRenderKeepsFoldedValuesThroughAnEdit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	long := strings.Repeat("word ", 40)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"NOTE:" + long[:60] + "\r\n " + long[60:] + "\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	note := propertyByNameAndGroup(envelope.PropertyTree, "NOTE", "")
	assert.Equal(long, note.Property.RawValue)

	fullName := propertyByNameAndGroup(envelope.PropertyTree, "FN", "")
	replacement, err := NewProperty("", "FN", "Alice Example")
	require.NoError(err)
	edited, err := envelope.MergeProperties([]PropertyEdit{{
		Identity: fullName.Identity, Property: replacement,
	}})
	require.NoError(err)
	canonical, err := edited.PrepareCanonicalRender()
	require.NoError(err)
	assert.Equal(long, propertyByNameAndGroup(canonical.PropertyTree, "NOTE", "").Property.RawValue)
	reparsed, err := ParseResourceEnvelope(canonical.StoredBody)
	require.NoError(err)
	assert.Equal(long, propertyByNameAndGroup(reparsed.PropertyTree, "NOTE", "").Property.RawValue)
	for line := range bytes.SplitSeq(canonical.StoredBody, []byte("\r\n")) {
		assert.LessOrEqual(len(line), 75)
	}
}

func TestV3ViewKeepsQuotedParameterValues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"EMAIL;X-KEEP=\"a,b\";TYPE=\"home\":alice@example.com\r\n" +
		"ADR;LABEL=\"1 Main St\\, Town\":;;1 Main St;Town;;;\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	v3, err := envelope.RenderView(Version30)
	require.NoError(err)
	assert.Contains(string(v3), "EMAIL;X-KEEP=\"a,b\";TYPE=home:alice@example.com\r\n")
	assert.Contains(string(v3), "ADR:;;1 Main St;Town;;;\r\n")
	reparsed, err := ParseResourceEnvelope(v3)
	require.NoError(err)
	email := propertyByNameAndGroup(reparsed.PropertyTree, "EMAIL", "")
	keep := email.Property.ParametersNamed("X-KEEP")
	require.Len(keep, 1)
	assert.Equal("a,b", keep[0].Values[0].Decoded)
}

func TestGeneratedV4CardsRoundTripAndCanonicalizeIdempotently(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	random := rand.New(rand.NewSource(20260818)) //nolint:gosec // deterministic seeded generator for a round-trip test
	for iteration := range 200 {
		raw := generateV4Card(random)
		envelope, err := ParseResourceEnvelope(raw)
		require.NoError(err, "iteration %d: %q", iteration, raw)

		rendered := forcedRender(t, envelope)
		reparsed, err := ParseResourceEnvelope(rendered)
		require.NoError(err, "iteration %d: %q", iteration, rendered)
		assertSameProperties(t, envelope.PropertyTree, reparsed.PropertyTree)

		envelope.RenderMetadata.RenderRequired = true
		first, err := envelope.PrepareCanonicalRender()
		require.NoError(err, "iteration %d: %q", iteration, raw)
		first.RenderMetadata.RenderRequired = true
		second, err := first.PrepareCanonicalRender()
		require.NoError(err, "iteration %d", iteration)
		assert.Equal(string(first.StoredBody), string(second.StoredBody), "iteration %d", iteration)
		assert.Equal(first.PropertyTree, second.PropertyTree, "iteration %d", iteration)

		v3, err := first.RenderView(Version30)
		require.NoError(err, "iteration %d: %q", iteration, first.StoredBody)
		_, err = ParseResourceEnvelope(v3)
		require.NoError(err, "iteration %d: %q", iteration, v3)
	}
}

func generateV4Card(random *rand.Rand) []byte {
	names := []string{"EMAIL", "TEL", "NOTE", "ADR", "N", "X-FOO", "URL", "BDAY", "GEO", "PHOTO"}
	values := []string{
		"alice@example.com", "tel:+12025550123", "+1 (202) 555-0123", "plain text",
		"escaped\\, comma\\; semicolon\\\\ backslash\\nnewline", ";;1 Main St;Town;;;",
		"Doe;Jane;;;", strings.Repeat("long value ", 20), "https://example.com/a?b=c&d=e",
		"19850412", "geo:37.386013,-122.082932", "data:image/png;base64,AA==",
		"Ünïcödé ✓ value", "",
	}
	parameters := []string{
		"TYPE=home", "TYPE=work,voice", "VALUE=text", "VALUE=uri", "PREF=1", "PID=1.1",
		"ALTID=1", "LANGUAGE=en", "X-KEEP=\"a,b\"", "X-CARET=A^^B^'C", "TYPE=pref",
		"MEDIATYPE=image/png", "LABEL=\"1 Main St\\, Town\"", "GEO=\"geo:1,2\"",
	}
	groups := []string{"", "", "item1", "ITEM2"}
	var card strings.Builder
	card.WriteString("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice Example\r\n")
	for range 1 + random.Intn(8) {
		if group := groups[random.Intn(len(groups))]; group != "" {
			card.WriteString(group + ".")
		}
		card.WriteString(names[random.Intn(len(names))])
		for range random.Intn(3) {
			card.WriteString(";" + parameters[random.Intn(len(parameters))])
		}
		card.WriteString(":" + values[random.Intn(len(values))] + "\r\n")
	}
	card.WriteString("END:VCARD\r\n")
	return []byte(card.String())
}

func assertSameProperties(t *testing.T, want, got []PropertyOccurrence) {
	t.Helper()
	require.Len(t, got, len(want))
	for index := range want {
		assert.Equal(t, want[index].Property, got[index].Property, "occurrence %d", index)
	}
}
