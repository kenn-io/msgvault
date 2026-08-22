package vcard

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseResourceEnvelopeKeepsRawBodyAndOccurrenceIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "v3-apple-groups.vcf"))
	require.NoError(err)

	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	assert.Equal(raw, envelope.OriginalRawBytes)
	assert.Equal(raw, envelope.StoredBody)
	assert.Equal(ContentHash(raw), envelope.ContentHash)
	assert.Equal(ETagForBody(raw), envelope.ETag)
	assert.Len(envelope.PropertyTree, 7)

	var tel, label PropertyOccurrence
	for _, occurrence := range envelope.PropertyTree {
		switch occurrence.Property.Name {
		case "TEL":
			tel = occurrence
		case "X-ABLABEL":
			label = occurrence
		}
	}
	require.NotEmpty(tel.Identity.Group)
	require.NotEmpty(label.Identity.Group)
	assert.Equal(tel.Identity.Group, label.Identity.Group)
	assert.NotEqual(tel.Identity.Ordinal, label.Identity.Ordinal)
	assert.Equal(tel.Identity, envelope.PropertyTree[4].Identity)
}

func TestResourceEnvelopeMergeUsesOccurrenceIdentityNotPropertyName(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice Example\r\n" +
		"item1.TEL;TYPE=home:+12025550123\r\n" +
		"item1.X-ABLABEL:Home\r\n" +
		"item2.TEL;TYPE=work;X-VENDOR=keep:+12025550124\r\n" +
		"item2.X-ABLABEL:Work\r\n" +
		"X-VENDOR;X-UNKNOWN=one,two:opaque\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)

	var target PropertyOccurrence
	for _, occurrence := range envelope.PropertyTree {
		if occurrence.Property.Name == "TEL" && occurrence.Identity.Group == "item2" {
			target = occurrence
			break
		}
	}
	require.NotEmpty(target.Identity)
	updated, err := NewProperty("item2", "TEL", "+12025550125")
	require.NoError(err)
	merged, err := envelope.MergeProperties([]PropertyEdit{{
		Identity: target.Identity,
		Property: updated,
	}})
	require.NoError(err)

	updatedOccurrence := propertyByIdentity(merged.PropertyTree, target.Identity)
	assert.Equal("+12025550125", updatedOccurrence.Property.RawValue)
	vendorParameters := updatedOccurrence.Property.ParametersNamed("X-VENDOR")
	require.Len(vendorParameters, 1)
	require.Len(vendorParameters[0].Values, 1)
	assert.Equal("keep", vendorParameters[0].Values[0].Decoded)
	assert.Equal("+12025550123",
		propertyByNameAndGroup(merged.PropertyTree, "TEL", "item1").Property.RawValue)
	assert.Equal("Work",
		propertyByNameAndGroup(merged.PropertyTree, "X-ABLABEL", "item2").Property.RawValue)
	require.Len(merged.Residue, 3)
	assert.Equal("X-ABLABEL", merged.Residue[0].Property.Name)
	assert.Equal("X-VENDOR", merged.Residue[2].Property.Name)
}

func TestMergePropertiesRemovesOwnedParametersOmittedByReplacement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice Example\r\n" +
		"N;LANGUAGE=de;SORT-AS=Beispiel,Alice;X-VENDOR=keep:Beispiel;Alice;;;\r\n" +
		"END:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	name := propertyByNameAndGroup(envelope.PropertyTree, "N", "")
	require.NotEmpty(name.Identity)

	replacement, err := NewProperty("", "N", "Example;Alice;;;")
	require.NoError(err)
	merged, err := envelope.MergeProperties([]PropertyEdit{{
		Identity:        name.Identity,
		Property:        replacement,
		OwnedParameters: []string{"LANGUAGE", "SCRIPT", "PHONETIC", "SORT-AS"},
	}})
	require.NoError(err)

	updated := propertyByIdentity(merged.PropertyTree, name.Identity)
	assert.Equal("Example;Alice;;;", updated.Property.RawValue)
	assert.Empty(updated.Property.ParametersNamed("LANGUAGE"))
	assert.Empty(updated.Property.ParametersNamed("SORT-AS"))
	vendor := updated.Property.ParametersNamed("X-VENDOR")
	require.Len(vendor, 1)
	require.Len(vendor[0].Values, 1)
	assert.Equal("keep", vendor[0].Values[0].Decoded)
}

func TestPrepareCanonicalRenderOfIdenticalEditDoesNotAdvanceRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice Example\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	name := propertyByNameAndGroup(envelope.PropertyTree, "FN", "")
	require.NotEmpty(name.Identity)
	replacement, err := NewProperty("", "FN", "Alice Changed")
	require.NoError(err)
	changed, err := envelope.MergeProperties([]PropertyEdit{{
		Identity: name.Identity, Property: replacement,
	}})
	require.NoError(err)
	first, err := changed.PrepareCanonicalRender()
	require.NoError(err)
	assert.False(first.RenderMetadata.RenderRequired)
	assert.Equal(envelope.RenderMetadata.Revision+1, first.RenderMetadata.Revision)

	// The same edit again renders to the stored bytes: it must settle the
	// render-required flag but not mint a revision nothing can distinguish.
	same, err := first.MergeProperties([]PropertyEdit{{
		Identity: name.Identity, Property: replacement,
	}})
	require.NoError(err)
	require.True(same.RenderMetadata.RenderRequired)
	second, err := same.PrepareCanonicalRender()
	require.NoError(err)
	assert.False(second.RenderMetadata.RenderRequired)
	assert.Equal(first.RenderMetadata.Revision, second.RenderMetadata.Revision)
	assert.Equal(first.StoredBody, second.StoredBody)
	assert.Equal(first.ETag, second.ETag)
	assert.Equal(first.PropertyTree, second.PropertyTree)
}

func TestMergePropertiesDropsTransferParametersOnEveryEdit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:2.1\r\nFN:Alice\r\n" +
		"X-NICK;CHARSET=ISO-8859-1;ENCODING=QUOTED-PRINTABLE;X-KEEP=yes:Caf=E9\r\n" +
		"X-MOTTO;QUOTED-PRINTABLE;HOME:Sant=E9\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	nick := propertyByNameAndGroup(envelope.PropertyTree, "X-NICK", "")
	motto := propertyByNameAndGroup(envelope.PropertyTree, "X-MOTTO", "")
	nickReplacement, err := NewProperty("", "X-NICK", "Café Résumé")
	require.NoError(err)
	mottoReplacement, err := NewProperty("", "X-MOTTO", "Santé")
	require.NoError(err)

	// Neither edit owns any parameter: an unmanaged property keeps what it
	// does not mention, except the declarations that described the old bytes.
	merged, err := envelope.MergeProperties([]PropertyEdit{
		{Identity: nick.Identity, Property: nickReplacement},
		{Identity: motto.Identity, Property: mottoReplacement},
	})
	require.NoError(err)
	updatedNick := propertyByIdentity(merged.PropertyTree, nick.Identity)
	assert.Empty(updatedNick.Property.ParametersNamed("CHARSET"))
	assert.Empty(updatedNick.Property.ParametersNamed("ENCODING"))
	require.Len(updatedNick.Property.ParametersNamed("X-KEEP"), 1)
	updatedMotto := propertyByIdentity(merged.PropertyTree, motto.Identity)
	for _, parameter := range updatedMotto.Property.Parameters {
		for _, value := range parameter.Values {
			assert.False(strings.EqualFold("QUOTED-PRINTABLE", value.Decoded),
				"bare 2.1 encoding token must not survive")
		}
	}
	require.Len(updatedMotto.Property.ParametersNamed("TYPE"), 1, "the bare HOME token stays as TYPE")

	v4, err := merged.RenderView(Version40)
	require.NoError(err)
	assert.Contains(string(v4), "X-NICK;X-KEEP=yes:Café Résumé\r\n")
	assert.Contains(string(v4), "X-MOTTO;TYPE=HOME:Santé\r\n")

	// A replacement cloned from the legacy occurrence carries the same
	// declarations itself; they are just as stale for its new plain value.
	cloned := cloneProperty(nick.Property)
	cloned.RawValue = "Café Résumé"
	mergedClone, err := envelope.MergeProperties([]PropertyEdit{
		{Identity: nick.Identity, Property: cloned},
		{Identity: motto.Identity, Property: mottoReplacement},
	})
	require.NoError(err)
	clonedNick := propertyByIdentity(mergedClone.PropertyTree, nick.Identity)
	assert.Empty(clonedNick.Property.ParametersNamed("CHARSET"))
	assert.Empty(clonedNick.Property.ParametersNamed("ENCODING"))
	require.Len(clonedNick.Property.ParametersNamed("X-KEEP"), 1)
	clonedView, err := mergedClone.RenderView(Version40)
	require.NoError(err)
	assert.Contains(string(clonedView), "X-NICK;X-KEEP=yes:Café Résumé\r\n")
}

func TestMergePropertiesReplacesOwnedParameterAndKeepsUnownedPosition(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice Example\r\n" +
		"ADR;LABEL=Old;CC=DE;X-VENDOR=keep:;;1 Old St;Town;;;\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	address := propertyByNameAndGroup(envelope.PropertyTree, "ADR", "")
	require.NotEmpty(address.Identity)

	replacement, err := NewProperty("", "ADR", ";;1 New St;Town;;;")
	require.NoError(err)
	label, err := NewParameter("LABEL", "New")
	require.NoError(err)
	replacement.Parameters = append(replacement.Parameters, label)
	merged, err := envelope.MergeProperties([]PropertyEdit{{
		Identity:        address.Identity,
		Property:        replacement,
		OwnedParameters: []string{"LABEL", "GEO", "TZ", "CC"},
	}})
	require.NoError(err)

	updated := propertyByIdentity(merged.PropertyTree, address.Identity)
	require.Len(updated.Property.Parameters, 2)
	assert.Equal("LABEL", updated.Property.Parameters[0].Name)
	assert.Equal("New", updated.Property.Parameters[0].Values[0].Decoded)
	assert.Equal("X-VENDOR", updated.Property.Parameters[1].Name)
	assert.Equal("keep", updated.Property.Parameters[1].Values[0].Decoded)
}

func TestMergePropertiesKeepsUndeclaredParametersOfUnmanagedProperty(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice Example\r\n" +
		"X-GENRE;VALUE=integer;X-VENDOR=keep:7\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	genre := propertyByNameAndGroup(envelope.PropertyTree, "X-GENRE", "")
	require.NotEmpty(genre.Identity)

	replacement, err := NewProperty("", "X-GENRE", "ambient")
	require.NoError(err)
	merged, err := envelope.MergeProperties([]PropertyEdit{{
		Identity: genre.Identity, Property: replacement,
	}})
	require.NoError(err)

	updated := propertyByIdentity(merged.PropertyTree, genre.Identity)
	assert.Equal([]Parameter{
		genre.Property.Parameters[0], genre.Property.Parameters[1],
	}, updated.Property.Parameters)
}

func TestResourceEnvelopeDeleteDropsOnlyMatchingNativeMapping(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice Example\r\n" +
		"EMAIL:alice@example.org\r\nTEL:+12025550123\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	var email, tel PropertyOccurrence
	for _, occurrence := range envelope.PropertyTree {
		switch occurrence.Property.Name {
		case "EMAIL":
			email = occurrence
		case "TEL":
			tel = occurrence
		}
	}
	envelope.NativeMappings = []NativeMapping{
		{Identity: email.Identity, Table: "person_contact_points", RowID: 1, Field: "email"},
		{Identity: tel.Identity, Table: "person_contact_points", RowID: 2, Field: "phone"},
	}
	merged, err := envelope.MergeProperties([]PropertyEdit{{Identity: email.Identity, Delete: true}})
	require.NoError(err)
	require.Len(merged.NativeMappings, 1)
	assert.Equal(tel.Identity, merged.NativeMappings[0].Identity)
}

func TestResourceEnvelopeV3ViewDoesNotMutateCanonicalV4Body(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice Example\r\n" +
		"PRONOUNS;LANGUAGE=en:she/her\r\n" +
		"ANNIVERSARY:20000101\r\n" +
		"UID:canonical-alice\r\n" +
		"CATEGORIES:friends,engineering\r\n" +
		"PRODID:-//example//contacts//EN\r\n" +
		"REV:20260811T000000Z\r\n" +
		"X-VENDOR;X-FUTURE=yes:keep me\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)

	v3, err := envelope.RenderView(Version30)
	require.NoError(err)
	assert.False(bytes.Contains(v3, []byte("PRONOUNS:")))
	assert.False(bytes.Contains(v3, []byte("ANNIVERSARY:")))
	assert.True(bytes.Contains(v3, []byte("UID:canonical-alice")))
	assert.True(bytes.Contains(v3, []byte("CATEGORIES:friends,engineering")))
	assert.True(bytes.Contains(v3, []byte("PRODID:-//example//contacts//EN")))
	assert.True(bytes.Contains(v3, []byte("REV:20260811T000000Z")))
	assert.Equal(raw, envelope.StoredBody)
	assert.Equal(ContentHash(raw), envelope.ContentHash)
	assert.Equal(ETagForBody(raw), envelope.ETag)

	v4, err := envelope.RenderView(Version40)
	require.NoError(err)
	assert.True(bytes.Contains(v4, []byte("PRONOUNS;LANGUAGE=en:she/her")))
	assert.True(bytes.Contains(v4, []byte("ANNIVERSARY:20000101")))
	assert.True(bytes.Contains(v4, []byte("X-VENDOR;X-FUTURE=yes:keep me")))
}

func TestResourceEnvelopePrepareCanonicalRenderStoresV4(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice Example\r\n" +
		"PRONOUNS;LANGUAGE=en:she/her\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)

	v3, err := envelope.RenderView(Version30)
	require.NoError(err)
	assert.Contains(string(v3), "VERSION:3.0")

	committed, err := envelope.PrepareCanonicalRender()
	require.NoError(err)
	assert.Equal(raw, committed.StoredBody)
	assert.Equal(raw, committed.OriginalRawBytes)
}

func TestResourceEnvelopeV4RenderNormalizesLegacyTextAndInlineMedia(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\n" +
		"FN;CHARSET=UTF-8;ENCODING=QUOTED-PRINTABLE:Ren=C3=A9 Dupont\r\n" +
		"PHOTO;ENCODING=b;TYPE=JPEG:/9j/4AAQSkZJRgABAQA=\r\n" +
		"END:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	v4, err := envelope.RenderView(Version40)
	require.NoError(err)
	assert.Contains(string(v4), "VERSION:4.0")
	assert.Contains(string(v4), "FN:René Dupont")
	assert.NotContains(string(v4), "CHARSET=")
	assert.NotContains(string(v4), "ENCODING=")
	assert.Contains(string(v4), "PHOTO:data:image/jpeg;base64,/9j/4AAQSkZJRgABAQA=")
	assert.Equal(raw, envelope.StoredBody)
}

func TestResourceEnvelopeV21ReadToV4NormalizesBareEncodingsAndStructure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:2.1\r\n" +
		"N;QUOTED-PRINTABLE:Doe;Jos=C3=A9;;;\r\n" +
		"PHOTO;BASE64;JPEG:/9j/4AAQSkZJRgABAQA=\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	_, err = envelope.RenderView(Version21)
	require.Error(err)
	v4, err := envelope.RenderView(Version40)
	require.NoError(err)
	assert.Contains(string(v4), "N:Doe;José;;;")
	assert.Contains(string(v4), "PHOTO:data:image/jpeg;base64,/9j/4AAQSkZJRgABAQA=")
	assert.NotContains(string(v4), "TYPE=QUOTED-PRINTABLE")
	assert.NotContains(string(v4), "TYPE=BASE64")
}

func TestResourceEnvelopeCanonicalizesLegacyCustomBase64AsDataURI(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Alice\r\n" +
		"X-BLOB;ENCODING=BASE64:AAEC/w==\r\n" +
		"X-TYPED;ENCODING=b;TYPE=JPEG:AAEC/w==\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)

	v4, err := envelope.RenderView(Version40)
	require.NoError(err)
	assert.Contains(string(v4),
		"X-BLOB;VALUE=uri:data:application/octet-stream;base64,AAEC/w==\r\n")
	assert.Contains(string(v4),
		"X-TYPED;TYPE=JPEG;VALUE=uri:data:application/octet-stream;base64,AAEC/w==\r\n",
		"a format token on a non-media property is a type, not a media type")
	assert.NotContains(string(v4), "ENCODING=")

	canonical, err := envelope.PrepareCanonicalRender()
	require.NoError(err)
	property := propertyByNameAndGroup(canonical.PropertyTree, "X-BLOB", "")
	assert.Equal("data:application/octet-stream;base64,AAEC/w==", property.Property.RawValue)
	valueParameters := property.Property.ParametersNamed("VALUE")
	require.Len(valueParameters, 1)
	require.Len(valueParameters[0].Values, 1)
	assert.Equal("uri", valueParameters[0].Values[0].Decoded)
}

func TestResourceEnvelopeV21QuotedPrintableLineBreaksBecomeTextEscapes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:2.1\r\nFN:Alice\r\n" +
		"NOTE;ENCODING=QUOTED-PRINTABLE:Line one=0D=0ALine two=0Aand three\r\n" +
		"ADR;ENCODING=QUOTED-PRINTABLE:;;1 Main St=0D=0ASuite 2;Town;;;\r\n" +
		"END:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)

	v4, err := envelope.RenderView(Version40)
	require.NoError(err)
	assert.Contains(string(v4), "NOTE:Line one\\nLine two\\nand three\r\n")
	assert.Contains(string(v4), "ADR:;;1 Main St\\nSuite 2;Town;;;\r\n")
	assert.NotContains(string(v4), "ENCODING=")
	v3, err := envelope.RenderView(Version30)
	require.NoError(err)
	assert.Contains(string(v3), "NOTE:Line one\\nLine two\\nand three\r\n")

	committed, err := envelope.PrepareCanonicalRender()
	require.NoError(err)
	note := propertyByNameAndGroup(committed.PropertyTree, "NOTE", "")
	text, err := UnescapeText(note.Property.RawValue)
	require.NoError(err)
	assert.Equal("Line one\nLine two\nand three", text)
	address := propertyByNameAndGroup(committed.PropertyTree, "ADR", "")
	assert.Equal(";;1 Main St\\nSuite 2;Town;;;", address.Property.RawValue)
}

func TestResourceEnvelopeKeepsKeyTextValueAcrossVersions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, source := range []Version{Version30, Version40} {
		raw := []byte("BEGIN:VCARD\r\nVERSION:" + string(source) + "\r\nFN:Alice\r\n" +
			"KEY;VALUE=text:-----BEGIN PGP PUBLIC KEY BLOCK-----\r\n" +
			"KEY:https://example.com/alice.asc\r\nEND:VCARD\r\n")
		envelope, err := ParseResourceEnvelope(raw)
		require.NoError(err)
		for _, target := range []Version{Version30, Version40} {
			view, err := envelope.RenderView(target)
			require.NoError(err, "%s -> %s", source, target)
			assert.Contains(string(view), "KEY;VALUE=text:-----BEGIN PGP PUBLIC KEY BLOCK-----\r\n",
				"%s -> %s must keep the text reset", source, target)
			assert.Contains(string(view), "https://example.com/alice.asc\r\n")
			assert.NotContains(string(view), "KEY;VALUE=text:https://", "%s -> %s", source, target)
		}
	}
}

func TestResourceEnvelopeDecodesMultibyteQuotedPrintableBeforeEscapingLineBreaks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	// UTF-16LE with BOM: "A", CR, LF, "B". The CR and LF bytes sit inside
	// 16-bit code units and must survive as such until the charset decode.
	raw := []byte("BEGIN:VCARD\r\nVERSION:2.1\r\nFN:Alice\r\n" +
		"NOTE;CHARSET=UTF-16;ENCODING=QUOTED-PRINTABLE:=FF=FE=41=00=0D=00=0A=00=42=00\r\n" +
		"END:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	v4, err := envelope.RenderView(Version40)
	require.NoError(err)
	assert.Contains(string(v4), "NOTE:A\\nB\r\n")
	assert.NotContains(string(v4), "CHARSET=")
}

func TestResourceEnvelopeDerivesFullNameForBothViewsWithoutV4OnlyParameter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	envelope, err := ParseResourceEnvelope(
		[]byte("BEGIN:VCARD\r\nVERSION:2.1\r\nN:Doe;Jane;;;\r\nEND:VCARD\r\n"),
	)
	require.NoError(err)
	v3, err := envelope.RenderView(Version30)
	require.NoError(err, "a card with only N must still have a vCard 3.0 view")
	assert.Contains(string(v3), "FN:Jane Doe\r\n")
	assert.NotContains(string(v3), "DERIVED")
	v4, err := envelope.RenderView(Version40)
	require.NoError(err)
	assert.Contains(string(v4), "FN;DERIVED=true:Jane Doe\r\n")
}

func TestResourceEnvelopeMovesLegacyReferencedMediaTypeToMediatype(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Alice\r\n" +
		"PHOTO;VALUE=uri;TYPE=JPEG:https://example.com/alice.jpg\r\n" +
		"LOGO;VALUE=uri;TYPE=png:https://example.com/logo.png\r\n" +
		"SOUND;VALUE=uri;TYPE=BASIC:https://example.com/hello.au\r\n" +
		"KEY;VALUE=uri;TYPE=PGP:https://example.com/alice.asc\r\n" +
		"END:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)

	v4, err := envelope.RenderView(Version40)
	require.NoError(err)
	body := string(v4)
	assert.Contains(body, "PHOTO;MEDIATYPE=image/jpeg:https://example.com/alice.jpg\r\n")
	assert.Contains(body, "LOGO;MEDIATYPE=image/png:https://example.com/logo.png\r\n")
	assert.Contains(body, "SOUND;MEDIATYPE=audio/basic:https://example.com/hello.au\r\n")
	assert.Contains(body, "KEY;MEDIATYPE=application/pgp-keys:https://example.com/alice.asc\r\n")
	assert.NotContains(body, ";TYPE=", "a vCard 3 media format token is not a vCard 4 context type")

	// The v4 body converts back with the format restored to TYPE, so the
	// media type survives a full round trip.
	canonical, err := envelope.PrepareCanonicalRender()
	require.NoError(err)
	v3, err := canonical.RenderView(Version30)
	require.NoError(err)
	assert.Contains(string(v3), "PHOTO;TYPE=JPEG;VALUE=uri:https://example.com/alice.jpg\r\n")
	assert.NotContains(string(v3), "MEDIATYPE")
}

func TestResourceEnvelopeConvertsTelephoneSyntaxAcrossV3AndV4(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	v3Raw := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Alice\r\n" +
		"TEL;TYPE=work,pref:+1 (202) 555-0123\r\nEND:VCARD\r\n")
	v3Envelope, err := ParseResourceEnvelope(v3Raw)
	require.NoError(err)

	v4, err := v3Envelope.RenderView(Version40)
	require.NoError(err)
	assert.Contains(string(v4), "TEL;TYPE=work;PREF=1:tel:+1(202)555-0123")

	v4Envelope, err := ParseResourceEnvelope(v4)
	require.NoError(err)
	v3, err := v4Envelope.RenderView(Version30)
	require.NoError(err)
	assert.Contains(string(v3), "TEL;TYPE=work,pref:+1(202)555-0123")
	assert.NotContains(string(v3), "VALUE=uri")
}

func TestResourceEnvelopeNormalizesLegacyGeoToGeoURI(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{
			name: "decimal coordinates", value: "37.386013;-122.082932",
			want: "GEO:geo:37.386013,-122.082932",
		},
		{
			name: "explicit positive sign", value: "+37.386013;-122.082932",
			want: "GEO:geo:37.386013,-122.082932",
		},
		{name: "integer coordinates", value: "37;-122", want: "GEO:geo:37,-122"},
		{
			name: "trailing zeros kept", value: "37.386000;-122.082930",
			want: "GEO:geo:37.386000,-122.082930",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			raw := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Alice\r\n" +
				"GEO:" + test.value + "\r\nEND:VCARD\r\n")
			envelope, err := ParseResourceEnvelope(raw)
			require.NoError(err)

			v4, err := envelope.RenderView(Version40)
			require.NoError(err)
			assert.Contains(string(v4), test.want)
			assert.NotContains(string(v4), "VALUE=")
			assert.Equal(raw, envelope.StoredBody)
		})
	}
}

func TestResourceEnvelopeRejectsLegacyGeoValueWithoutGeoURISpelling(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "single component", value: "37.386013"},
		{name: "empty value", value: ""},
		{name: "non-numeric components", value: "north;west"},
		{name: "latitude out of range", value: "91;-122.082932"},
		{name: "longitude out of range", value: "37.386013;-181"},
		{name: "exponent notation", value: "3.7386013e1;-122.082932"},
		{name: "three components", value: "37.386013;-122.082932;250"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Alice\r\n" +
				"GEO:" + test.value + "\r\nEND:VCARD\r\n")
			envelope, err := ParseResourceEnvelope(raw)
			require.NoError(t, err)

			_, err = envelope.RenderView(Version40)
			require.ErrorContains(t, err, "cannot be converted to a geo URI")
			assert.Equal(t, raw, envelope.StoredBody)
		})
	}
}

func TestResourceEnvelopeV3ViewRendersOnlyConvertibleGeoURIs(t *testing.T) {
	tests := []struct {
		name   string
		value  string
		wantV3 string
	}{
		{
			name: "plain coordinates", value: "geo:37.386013,-122.082932",
			wantV3: "GEO:37.386013;-122.082932",
		},
		{
			name: "uppercase scheme", value: "GEO:37.386013,-122.082932",
			wantV3: "GEO:37.386013;-122.082932",
		},
		{
			name: "default crs", value: "geo:37.386013,-122.082932;crs=wgs84",
			wantV3: "GEO:37.386013;-122.082932",
		},
		{name: "other crs", value: "geo:37.386013,-122.082932;crs=Moon"},
		{name: "uncertainty", value: "geo:37.386013,-122.082932;u=35"},
		{name: "altitude", value: "geo:37.386013,-122.082932,250"},
		{name: "other scheme", value: "https://example.com/places/exampleville"},
		{name: "coordinates out of range", value: "geo:137.386013,-122.082932"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
				"GEO:" + test.value + "\r\nEND:VCARD\r\n")
			envelope, err := ParseResourceEnvelope(raw)
			require.NoError(err)
			fullName := propertyByNameAndGroup(envelope.PropertyTree, "FN", "")
			replacement, err := NewProperty("", "FN", "Alice Example")
			require.NoError(err)
			edited, err := envelope.MergeProperties([]PropertyEdit{{
				Identity: fullName.Identity, Property: replacement,
			}})
			require.NoError(err)

			v3, err := edited.RenderView(Version30)
			require.NoError(err)
			if test.wantV3 != "" {
				assert.Contains(string(v3), test.wantV3)
				assert.NotContains(string(v3), "VALUE=uri")
			} else {
				assert.NotContains(string(v3), "GEO:")
			}

			// The canonical tree keeps the value a vCard 3 view cannot spell,
			// so a rendered vCard 4 view still carries it.
			assert.Equal(test.value,
				propertyByNameAndGroup(edited.PropertyTree, "GEO", "").Property.RawValue)
			v4, err := edited.RenderView(Version40)
			require.NoError(err)
			assert.Contains(string(v4), "GEO:"+test.value)
		})
	}
}

func TestResourceEnvelopeGeoRoundTripsThroughCanonicalV4(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Alice\r\n" +
		"GEO:37.386013;-122.082932\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)

	committed, err := envelope.PrepareCanonicalRender()
	require.NoError(err)
	assert.Contains(string(committed.StoredBody), "GEO:geo:37.386013,-122.082932")

	v3, err := committed.RenderView(Version30)
	require.NoError(err)
	assert.Contains(string(v3), "GEO:37.386013;-122.082932")
	assert.Equal(raw, committed.OriginalRawBytes)
}

func TestResourceEnvelopeV3ViewConvertsMediaTypeAndDropsV4Parameters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"PHOTO;MEDIATYPE=image/jpeg;PROP-ID=p1:https://example.com/alice.jpg\r\n" +
		"N;SORT-AS=Public,John;ALTID=1;PROP-ID=n1:Public;John;;;Jr.;Smith;II\r\n" +
		"ADR;LABEL=Home;GEO=\"geo:1,2\";TZ=UTC;CC=US:" +
		";;1 Main St;Town;CA;90210;US;;;;1;Main St;;;;;;\r\n" +
		"END:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)

	v3, err := envelope.RenderView(Version30)
	require.NoError(err)
	assert.Contains(string(v3), "PHOTO;TYPE=JPEG;VALUE=uri:https://example.com/alice.jpg")
	assert.Contains(string(v3), "N:Public;John;;;Jr.")
	assert.Contains(string(v3), "ADR:;;1 Main St;Town;CA;90210;US")
	for _, parameter := range []string{
		"PROP-ID=", "ALTID=", "SORT-AS=", "LABEL=", "GEO=", "TZ=", "CC=",
		"MEDIATYPE=",
	} {
		assert.NotContains(string(v3), parameter)
	}

	v4, err := envelope.RenderView(Version40)
	require.NoError(err)
	assert.Equal(raw, v4)
}

func TestPrepareCanonicalRenderFromLegacyUpdatesBodyHashAndETagTogether(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Alice\r\n" +
		"TEL;TYPE=pref:+12025550123\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)

	committed, err := envelope.PrepareCanonicalRender()
	require.NoError(err)
	assert.Equal(raw, committed.OriginalRawBytes)
	assert.Contains(string(committed.StoredBody), "VERSION:4.0")
	assert.Contains(string(committed.StoredBody), "TEL;PREF=1:tel:+12025550123")
	assert.Equal(ContentHash(committed.StoredBody), committed.ContentHash)
	assert.Equal(ETagForBody(committed.StoredBody), committed.ETag)
}

func TestParseResourceEnvelopeRejectsMalformedLineWithoutPartialCard(t *testing.T) {
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice Example\r\nBROKEN\r\nEND:VCARD\r\n")
	_, err := ParseResourceEnvelope(raw)
	require.Error(err)
	var parseErr *ParseError
	require.ErrorAs(err, &parseErr)
}

func TestResourceEnvelopeNoRenderReturnsStoredBytesExactly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Alice Example\r\n" +
		"NOTE:line 1\r\n line 2\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)

	got, err := envelope.RenderView(Version30)
	require.NoError(err)
	assert.True(bytes.Equal(raw, got))
}

func TestCommitRenderedBodyKeepsWireOrderAndStableOccurrenceIdentities(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice Example\r\n" +
		"EMAIL:alice@example.org\r\nTEL:+12025550123\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	email := propertyByNameAndGroup(envelope.PropertyTree, "EMAIL", "")
	tel := propertyByNameAndGroup(envelope.PropertyTree, "TEL", "")
	envelope.NativeMappings = []NativeMapping{
		{Identity: email.Identity, Table: "person_contact_points", RowID: 1, Field: "email"},
		{Identity: tel.Identity, Table: "person_contact_points", RowID: 2, Field: "phone"},
	}

	committedBody := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nTEL:+12025550123\r\n" +
		"NOTE:inserted\r\nFN:Alice Example\r\nEMAIL:alice@example.org\r\nEND:VCARD\r\n")
	committed, err := envelope.commitRenderedBody(committedBody)
	require.NoError(err)
	require.Len(committed.PropertyTree, 5)
	assert.Equal([]string{"VERSION", "TEL", "NOTE", "FN", "EMAIL"}, []string{
		committed.PropertyTree[0].Property.Name,
		committed.PropertyTree[1].Property.Name,
		committed.PropertyTree[2].Property.Name,
		committed.PropertyTree[3].Property.Name,
		committed.PropertyTree[4].Property.Name,
	})
	assert.Equal(tel.Identity, committed.PropertyTree[1].Identity)
	assert.Equal(email.Identity, committed.PropertyTree[4].Identity)
	assert.Equal(4, committed.PropertyTree[2].Identity.Ordinal)
	assert.Equal(committedBody, committed.StoredBody)
	require.Len(committed.NativeMappings, 2)
	assert.Equal(tel.Identity, committed.NativeMappings[1].Identity)
}

func TestMergePropertiesDoesNotReuseDeletedHighWaterOrdinal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"EMAIL:a@example.com\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	highest := envelope.PropertyTree[len(envelope.PropertyTree)-1].Identity.Ordinal

	withoutEnd, err := envelope.MergeProperties([]PropertyEdit{{
		Identity: envelope.PropertyTree[len(envelope.PropertyTree)-1].Identity,
		Delete:   true,
	}})
	require.NoError(err)
	note, err := NewProperty("", "NOTE", "new")
	require.NoError(err)
	withNote, err := withoutEnd.MergeProperties([]PropertyEdit{{Property: note}})
	require.NoError(err)

	assert.Greater(
		withNote.PropertyTree[len(withNote.PropertyTree)-1].Identity.Ordinal,
		highest,
	)
}

func TestReconcilePropertyTreeReusesStableIdentityOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	stable, err := ParseResourceEnvelope([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\n" +
		"FN:Alice Example\r\nEMAIL:alice@example.org\r\nEND:VCARD\r\n"))
	require.NoError(err)
	wire, err := ParseResourceEnvelope([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\n" +
		"FN:Alice Example\r\nEMAIL:alice@example.org\r\n" +
		"EMAIL:alice@example.org\r\nEND:VCARD\r\n"))
	require.NoError(err)

	reconciled := reconcilePropertyTree(stable.PropertyTree, wire.PropertyTree)
	require.Len(reconciled, 4)
	assert.Equal(stable.PropertyTree[2].Identity, reconciled[2].Identity)
	assert.NotEqual(reconciled[2].Identity, reconciled[3].Identity)
	assert.Equal(3, reconciled[3].Identity.Ordinal)
}

func TestRenderViewDoesNotMutateLegacyPropertySyntax(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:2.1\r\nFN:Alice Example\r\n" +
		"TEL;HOME;PID=1.1:+12025550123\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	want, err := ParseResourceEnvelope(raw)
	require.NoError(err)

	_, err = envelope.RenderView(Version40)
	require.NoError(err)
	assert.Equal(want.PropertyTree, envelope.PropertyTree)
	assert.Equal(want.Residue, envelope.Residue)
}

func TestMergePropertiesDeepCopiesNestedOccurrenceState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice Example\r\n" +
		"EMAIL;PID=1.1;ALTID=a;PROP-ID=p:alice@example.org\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	email := propertyByNameAndGroup(envelope.PropertyTree, "EMAIL", "")
	envelope.NativeMappings = []NativeMapping{{
		Identity: email.Identity, Table: "person_contact_points", RowID: 1, Field: "email",
	}}
	envelope.Residue = []PropertyOccurrence{email}

	cloned, err := envelope.MergeProperties(nil)
	require.NoError(err)
	clonedEmail := propertyByIdentity(cloned.PropertyTree, email.Identity)
	require.NotEmpty(clonedEmail.Property.Parameters)
	clonedEmail.Property.Parameters[0].Values[0].Decoded = "changed"
	*clonedEmail.Identity.PropID = "changed"
	clonedEmail.Identity.PID[0] = "changed"
	*clonedEmail.Identity.AltID = "changed"
	cloned.NativeMappings[0].Identity.PID[0] = "changed"
	cloned.Residue[0].Property.Parameters[0].Values[0].Decoded = "changed"

	originalEmail := propertyByIdentity(envelope.PropertyTree, email.Identity)
	assert.Equal("1.1", originalEmail.Property.Parameters[0].Values[0].Decoded)
	assert.Equal("p", *originalEmail.Identity.PropID)
	assert.Equal([]string{"1.1"}, originalEmail.Identity.PID)
	assert.Equal("a", *originalEmail.Identity.AltID)
	assert.Equal([]string{"1.1"}, envelope.NativeMappings[0].Identity.PID)
	assert.Equal("1.1", envelope.Residue[0].Property.Parameters[0].Values[0].Decoded)
}

func TestResourceMetadataUsesVersionedStableJSONAndRoundTrips(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\n" +
		"item1.EMAIL;TYPE=home;X-KEEP=\"mixed value\":alice@example.com\r\n" +
		"END:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	email := propertyByNameAndGroup(envelope.PropertyTree, "EMAIL", "item1")
	envelope.NativeMappings = []NativeMapping{{
		Identity: email.Identity, Table: "person_contact_points", RowID: 7,
		Field: "email", Kind: HandlingNative,
	}}
	envelope.Residue = ResidueWithMappings(envelope.PropertyTree, envelope.NativeMappings)

	data, err := MarshalResourceMetadata(envelope)
	require.NoError(err)
	assert.Contains(string(data), `"format_version":1`)
	assert.Contains(string(data), `"raw_value":"alice@example.com"`)
	assert.NotContains(string(data), `"RawValue"`)
	assert.True(json.Valid(data))

	decoded, err := UnmarshalResourceMetadata(data)
	require.NoError(err)
	assert.Equal(envelope.PropertyTree, decoded.PropertyTree)
	assert.Equal(envelope.NativeMappings, decoded.NativeMappings)
	assert.Equal(envelope.Residue, decoded.Residue)
	assert.Equal(envelope.RenderMetadata, decoded.RenderMetadata)
	assert.Equal(envelope.NextOccurrenceOrdinal, decoded.NextOccurrenceOrdinal)
}

func TestResourceMetadataRejectsUnknownVersionAndStaleHighWater(t *testing.T) {
	require := require.New(t)
	envelope, err := ParseResourceEnvelope([]byte(
		"BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n",
	))
	require.NoError(err)
	data, err := MarshalResourceMetadata(envelope)
	require.NoError(err)

	var document map[string]any
	require.NoError(json.Unmarshal(data, &document))
	document["format_version"] = 2
	unknownVersion, err := json.Marshal(document)
	require.NoError(err)
	_, err = UnmarshalResourceMetadata(unknownVersion)
	require.ErrorContains(err, "unsupported vCard resource metadata format 2")

	envelope.NextOccurrenceOrdinal = 0
	_, err = MarshalResourceMetadata(envelope)
	require.ErrorContains(err, "high-water mark is stale")
}

func TestResourceMetadataRejectsAmbiguousMappingsAndDivergentResidue(t *testing.T) {
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"EMAIL:one@example.com\r\nEMAIL:two@example.com\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	first := propertyByNameAndValue(envelope.PropertyTree, "EMAIL", "one@example.com")
	second := propertyByNameAndValue(envelope.PropertyTree, "EMAIL", "two@example.com")
	envelope.NativeMappings = []NativeMapping{
		{
			Identity: first.Identity, Table: "person_contact_points", RowID: 1,
			Field: "value", Kind: HandlingNative,
		},
		{
			Identity: first.Identity, Table: "person_contact_points", RowID: 2,
			Field: "value", Kind: HandlingNative,
		},
	}
	_, err = MarshalResourceMetadata(envelope)
	require.ErrorContains(err, "claimed by multiple native mappings")

	envelope.NativeMappings[1].Identity = second.Identity
	envelope.NativeMappings[1].RowID = 1
	_, err = MarshalResourceMetadata(envelope)
	require.ErrorContains(err, "native owner has multiple occurrence mappings")

	envelope.NativeMappings = nil
	divergent := first
	divergent.Property.RawValue = "different@example.com"
	envelope.Residue = []PropertyOccurrence{divergent}
	_, err = MarshalResourceMetadata(envelope)
	require.ErrorContains(err, "does not match the property tree")
}

func propertyByIdentity(
	properties []PropertyOccurrence, identity PropertyIdentity,
) PropertyOccurrence {
	for _, property := range properties {
		if property.Identity.Equal(identity) {
			return property
		}
	}
	return PropertyOccurrence{}
}

func propertyByNameAndValue(
	properties []PropertyOccurrence, name, rawValue string,
) PropertyOccurrence {
	for _, property := range properties {
		if property.Property.Name == name && property.Property.RawValue == rawValue {
			return property
		}
	}
	return PropertyOccurrence{}
}

func propertyByNameAndGroup(
	properties []PropertyOccurrence, name, group string,
) PropertyOccurrence {
	for _, property := range properties {
		if property.Property.Name == name && property.Property.Group == group {
			return property
		}
	}
	return PropertyOccurrence{}
}

func TestResourceMetadataRoundTripsNonUTF8RawValues(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nN;CHARSET=ISO-8859-1:Caf\xe9;;;;\r\n" +
		"FN:Cafe\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	name := propertyByNameAndGroup(envelope.PropertyTree, "N", "")
	require.Equal("Caf\xe9;;;;", name.Property.RawValue)

	data, err := MarshalResourceMetadata(envelope)
	require.NoError(err)
	assert.Contains(string(data), `"raw_value_base64":"`)
	assert.NotContains(string(data), "�")
	decoded, err := UnmarshalResourceMetadata(data)
	require.NoError(err)
	assert.Equal(envelope.PropertyTree, decoded.PropertyTree)
	assert.Equal(envelope.Residue, decoded.Residue)

	v4, err := decoded.RenderView(Version40)
	require.NoError(err)
	assert.Contains(string(v4), "N:Café;;;;\r\n")
}

func TestPrepareCanonicalRenderKeepsMappingWhenEditChangesWireIdentity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"EMAIL;PID=1.1:alice@example.org\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	email := propertyByNameAndGroup(envelope.PropertyTree, "EMAIL", "")
	envelope.NativeMappings = []NativeMapping{{
		Identity: email.Identity, Table: "person_contact_points", RowID: 1,
		Field: "email", Kind: HandlingNative,
	}}

	replacement, err := NewProperty("", "EMAIL", "alice@example.org")
	require.NoError(err)
	pid, err := NewParameter("PID", "1.2")
	require.NoError(err)
	replacement.Parameters = append(replacement.Parameters, pid)
	merged, err := envelope.MergeProperties([]PropertyEdit{{
		Identity: email.Identity, Property: replacement,
	}})
	require.NoError(err)
	canonical, err := merged.PrepareCanonicalRender()
	require.NoError(err)
	assert.Contains(string(canonical.StoredBody), "EMAIL;PID=1.2:alice@example.org\r\n")

	updated := propertyByNameAndGroup(canonical.PropertyTree, "EMAIL", "")
	assert.Equal(email.Identity.Ordinal, updated.Identity.Ordinal)
	assert.Equal([]string{"1.2"}, updated.Identity.PID)
	require.Len(canonical.NativeMappings, 1)
	assert.Equal(updated.Identity, canonical.NativeMappings[0].Identity)
	assert.Len(canonical.PropertyTree, len(envelope.PropertyTree))
	_, err = MarshalResourceMetadata(canonical)
	require.NoError(err)
}

func TestMergePropertiesRejectsMalformedEdits(t *testing.T) {
	require := require.New(t)
	envelope, err := ParseResourceEnvelope(
		[]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\nEND:VCARD\r\n"),
	)
	require.NoError(err)
	fullName := propertyByNameAndGroup(envelope.PropertyTree, "FN", "")

	_, err = envelope.MergeProperties([]PropertyEdit{{Delete: true}})
	require.ErrorContains(err, "delete edit requires an occurrence identity")

	_, err = envelope.MergeProperties([]PropertyEdit{{Property: Property{RawValue: "x"}}})
	require.ErrorContains(err, "empty property name")

	_, err = envelope.MergeProperties([]PropertyEdit{{
		Identity: fullName.Identity, Property: Property{Name: "bad name", RawValue: "x"},
	}})
	require.ErrorContains(err, `invalid property name "bad name"`)

	missing := fullName.Identity
	missing.Ordinal = 99
	replacement, err := NewProperty("", "FN", "Bob")
	require.NoError(err)
	_, err = envelope.MergeProperties([]PropertyEdit{{Identity: missing, Property: replacement}})
	require.ErrorContains(err, "property edit identity not found")

	_, err = envelope.MergeProperties([]PropertyEdit{
		{Identity: fullName.Identity, Property: replacement},
		{Identity: fullName.Identity, Property: replacement},
	})
	require.ErrorContains(err, "duplicate property edit identity")
	require.Len(envelope.PropertyTree, 2, "failed merges leave the envelope untouched")
}

func TestReconcilePropertyTreeMatchesGroupsCaseInsensitively(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	raw := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"item1.TEL;PID=1.1:tel:+12025550123\r\nitem1.X-ABLABEL:Home\r\nEND:VCARD\r\n")
	envelope, err := ParseResourceEnvelope(raw)
	require.NoError(err)
	tel := propertyByNameAndGroup(envelope.PropertyTree, "TEL", "item1")
	envelope.NativeMappings = []NativeMapping{{
		Identity: tel.Identity, Table: "person_contact_points", RowID: 1,
		Field: "phone", Kind: HandlingNative,
	}}

	body := []byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:Alice\r\n" +
		"ITEM1.TEL;PID=1.1:tel:+12025550124\r\nITEM1.X-ABLABEL:Home\r\nEND:VCARD\r\n")
	committed, err := envelope.commitRenderedBody(body)
	require.NoError(err)
	updated := propertyByNameAndGroup(committed.PropertyTree, "TEL", "ITEM1")
	assert.Equal(tel.Identity.Ordinal, updated.Identity.Ordinal)
	require.Len(committed.NativeMappings, 1)
	assert.Equal(updated.Identity, committed.NativeMappings[0].Identity)
	assert.Len(committed.PropertyTree, len(envelope.PropertyTree))
	assert.Equal(nextPropertyOrdinal(envelope.PropertyTree), committed.NextOccurrenceOrdinal)
}

func TestPropertyIdentityEqualComparesEveryComponent(t *testing.T) {
	assert := assert.New(t)
	base := PropertyIdentity{
		Ordinal: 3, Group: "item1", OriginalName: "TEL",
		PropID: new("p"), PID: []string{"1.1", "2.1"}, AltID: new("a"),
	}
	same := clonePropertyIdentity(base)
	assert.True(base.Equal(same))
	assert.True(same.Equal(base))
	for name, mutate := range map[string]func(*PropertyIdentity){
		"ordinal": func(i *PropertyIdentity) { i.Ordinal = 4 },
		"group":   func(i *PropertyIdentity) { i.Group = "item2" },
		"name":    func(i *PropertyIdentity) { i.OriginalName = "tel" },
		"prop-id": func(i *PropertyIdentity) { i.PropID = nil },
		"pid":     func(i *PropertyIdentity) { i.PID = []string{"1.1"} },
		"altid":   func(i *PropertyIdentity) { *i.AltID = "b" },
	} {
		other := clonePropertyIdentity(base)
		mutate(&other)
		assert.False(base.Equal(other), name)
		assert.NotEqual(base.Key(), other.Key(), name)
	}
}

func TestParseResourceEnvelopeRejectsEmptyAndMultiCardBodies(t *testing.T) {
	require := require.New(t)
	_, err := ParseResourceEnvelope(nil)
	require.ErrorContains(err, "empty")
	_, err = ParseResourceEnvelope([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\nFN:A\r\nEND:VCARD\r\n" +
		"BEGIN:VCARD\r\nVERSION:4.0\r\nFN:B\r\nEND:VCARD\r\n"))
	require.ErrorContains(err, "exactly one card, found 2")
	_, err = ParseResourceEnvelope([]byte("BEGIN:VCARD\r\nVERSION:5.0\r\nFN:A\r\nEND:VCARD\r\n"))
	require.ErrorContains(err, `unsupported VERSION "5.0"`)
}

func TestResourceMetadataRejectsInconsistentOccurrences(t *testing.T) {
	require := require.New(t)
	parse := func() ResourceEnvelope {
		envelope, err := ParseResourceEnvelope([]byte("BEGIN:VCARD\r\nVERSION:4.0\r\n" +
			"FN:Alice\r\nitem1.EMAIL;PROP-ID=e1:a@example.com\r\nEND:VCARD\r\n"))
		require.NoError(err)
		return envelope
	}
	reject := func(name, message string, tamper func(envelope *ResourceEnvelope)) {
		envelope := parse()
		tamper(&envelope)
		// Keep the residue consistent with the tampered tree so the check
		// under test, not the residue comparison, is what rejects it.
		envelope.Residue = ResidueWithMappings(envelope.PropertyTree, envelope.NativeMappings)
		_, err := MarshalResourceMetadata(envelope)
		require.ErrorContains(err, message, name)
		valid, err := MarshalResourceMetadata(parse())
		require.NoError(err, name)
		var wire map[string]any
		require.NoError(json.Unmarshal(valid, &wire), name)
		tampered := parse()
		tamper(&tampered)
		tampered.Residue = ResidueWithMappings(tampered.PropertyTree, tampered.NativeMappings)
		wire["property_tree"] = tampered.PropertyTree
		wire["residue"] = tampered.Residue
		raw, err := json.Marshal(wire)
		require.NoError(err, name)
		_, err = UnmarshalResourceMetadata(raw)
		require.ErrorContains(err, message, name)
	}

	// Ordinal-indexed merge and residue logic would silently attach mappings
	// and residue to the wrong occurrence for any of these.
	reject("duplicate ordinal", "duplicate vCard occurrence ordinal",
		func(envelope *ResourceEnvelope) {
			envelope.PropertyTree[2].Identity.Ordinal =
				envelope.PropertyTree[1].Identity.Ordinal
		})
	reject("negative ordinal", "negative vCard occurrence ordinal",
		func(envelope *ResourceEnvelope) {
			envelope.PropertyTree[2].Identity.Ordinal = -1
		})
	reject("identity mismatch", "identity does not match its property",
		func(envelope *ResourceEnvelope) {
			envelope.PropertyTree[2].Identity.Group = "item9"
		})
	reject("classification mismatch", "classification does not match its property",
		func(envelope *ResourceEnvelope) {
			envelope.PropertyTree[2].Classification = HandlingPreserve
		})
}
