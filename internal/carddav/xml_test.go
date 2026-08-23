package carddav

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseMultiStatusRejectsDoctypeAndDepthOverflow(t *testing.T) {
	_, err := ParseMultiStatus([]byte(`<!DOCTYPE x><multistatus/>`), DefaultXMLLimits())
	require.ErrorIs(t, err, ErrUnsafeXML)
	deep := strings.Repeat("<x>", 65) + strings.Repeat("</x>", 65)
	_, err = ParseMultiStatus([]byte(deep), DefaultXMLLimits())
	require.ErrorIs(t, err, ErrXMLLimit)
}

func TestParseMultiStatusKeepsResponseAndPropstatStatus(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	multi, err := ParseMultiStatus([]byte(cardDAVMultiStatusFixture), DefaultXMLLimits())
	require.NoError(err)
	require.Len(multi.Responses, 1)
	assert.Equal("/dav/books/personal/alice.vcf", multi.Responses[0].Href)
	require.Len(multi.Responses[0].PropStats, 1)
	assert.Equal(200, multi.Responses[0].PropStats[0].StatusCode)
	assert.Equal("\"abc\"", multi.Responses[0].PropStats[0].Properties.GetETag)
	assert.Equal("BEGIN:VCARD\nVERSION:4.0\nEND:VCARD", multi.Responses[0].PropStats[0].Properties.AddressData)
}

func TestParseMultiStatusPreservesAddressDataAndRejectsWrongNamespaces(t *testing.T) {
	const exactAddressData = "  BEGIN:VCARD\nVERSION:4.0\nEND:VCARD  "
	body := []byte(`<D:multistatus xmlns:D="DAV:" xmlns:CR="urn:ietf:params:xml:ns:carddav"><D:response><D:href>/alice.vcf</D:href><D:propstat><D:prop><CR:address-data>` + exactAddressData + `</CR:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`)
	multi, err := ParseMultiStatus(body, DefaultXMLLimits())
	require.NoError(t, err)
	assert.Equal(t, exactAddressData, multi.Responses[0].PropStats[0].Properties.AddressData)

	for _, wrongNamespace := range [][]byte{
		[]byte(`<X:multistatus xmlns:X="https://example.invalid"/>`),
		[]byte(`<D:multistatus xmlns:D="DAV:" xmlns:X="https://example.invalid"><D:response><D:href>/alice.vcf</D:href><D:propstat><D:prop><X:getetag>spoof</X:getetag></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`),
		[]byte(`<D:multistatus xmlns:D="DAV:" xmlns:X="https://example.invalid"><D:response><D:href>/alice.vcf</D:href><D:propstat><D:prop><X:address-data>spoof</X:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`),
	} {
		_, err := ParseMultiStatus(wrongNamespace, DefaultXMLLimits())
		require.ErrorIs(t, err, ErrUnsafeXML)
	}
}

func TestParseMultiStatusAddressDataPreservesLinesAndDecodesXMLEntities(t *testing.T) {
	const encoded = "BEGIN:VCARD\r\nNOTE:&amp;&lt;&gt;&apos;&quot;&#35;&#x40;\rEND:VCARD\r\n"
	const expected = "BEGIN:VCARD\r\nNOTE:&<>'\"#@\rEND:VCARD\r\n"
	body := []byte(`<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:response><D:href>/alice.vcf</D:href><D:propstat><D:prop><C:address-data>` + encoded + `</C:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`)
	multi, err := ParseMultiStatus(body, DefaultXMLLimits())
	require.NoError(t, err)
	assert.Equal(t, expected, multi.Responses[0].PropStats[0].Properties.AddressData)
}

func TestParseMultiStatusAddressDataPreservesCDATABytes(t *testing.T) {
	const encoded = "BEGIN:VCARD\r\nNOTE:ordinary&amp;<![CDATA[<literal>&amp;\r\n]]><![CDATA[more\r]]>END:VCARD\r\n"
	const expected = "BEGIN:VCARD\r\nNOTE:ordinary&<literal>&amp;\r\nmore\rEND:VCARD\r\n"
	body := []byte(`<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:response><D:href>/alice.vcf</D:href><D:propstat><D:prop><C:address-data>` + encoded + `</C:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`)
	multi, err := ParseMultiStatus(body, DefaultXMLLimits())
	require.NoError(t, err)
	assert.Equal(t, expected, multi.Responses[0].PropStats[0].Properties.AddressData)
}

func TestParseMultiStatusRejectsMarkupInsideAddressData(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
	}{
		{name: "child element", content: `BEGIN:VCARD<D:href>nested</D:href>END:VCARD`},
		{name: "comment", content: `BEGIN:VCARD<!--hidden-->END:VCARD`},
		{name: "processing instruction", content: `BEGIN:VCARD<?hidden value?>END:VCARD`},
		{name: "declaration", content: `BEGIN:VCARD<!DOCTYPE hidden>END:VCARD`},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:response><D:href>/alice.vcf</D:href><D:propstat><D:prop><C:address-data>` + test.content + `</C:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`)
			_, err := ParseMultiStatus(body, DefaultXMLLimits())
			require.ErrorIs(t, err, ErrUnsafeXML)
		})
	}

	unterminated := []byte(`<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:response><D:href>/alice.vcf</D:href><D:propstat><D:prop><C:address-data><![CDATA[BEGIN:VCARD</C:address-data></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`)
	_, err := ParseMultiStatus(unterminated, DefaultXMLLimits())
	require.Error(t, err)
}

func TestParseMultiStatusRejectsByteLimit(t *testing.T) {
	_, err := ParseMultiStatus([]byte(`<D:multistatus xmlns:D="DAV:"/>`), XMLLimits{MaxBytes: 8, MaxDepth: 64})
	require.ErrorIs(t, err, ErrXMLLimit)
}

func TestParseMultiStatusRejectsStructuralAmplificationBeforeUnmarshal(t *testing.T) {
	response := `<D:response><D:href>/a.vcf</D:href></D:response>`
	body := []byte(`<D:multistatus xmlns:D="DAV:">` + strings.Repeat(response, 3) + `</D:multistatus>`)
	limits := DefaultXMLLimits()
	limits.MaxResponses = 2
	_, err := ParseMultiStatus(body, limits)
	require.ErrorIs(t, err, ErrXMLLimit)

	propStat := `<D:propstat><D:prop/><D:status>HTTP/1.1 200 OK</D:status></D:propstat>`
	body = []byte(`<D:multistatus xmlns:D="DAV:"><D:response><D:href>/a.vcf</D:href>` +
		strings.Repeat(propStat, 3) + `</D:response></D:multistatus>`)
	limits = DefaultXMLLimits()
	limits.MaxPropStats = 2
	_, err = ParseMultiStatus(body, limits)
	require.ErrorIs(t, err, ErrXMLLimit)

	body = []byte(`<D:multistatus xmlns:D="DAV:"><D:response><D:href>/a.vcf</D:href>` +
		propStat + `</D:response></D:multistatus>`)
	limits = DefaultXMLLimits()
	limits.MaxElements = 4
	_, err = ParseMultiStatus(body, limits)
	require.ErrorIs(t, err, ErrXMLLimit)
}

func TestParseMultiStatusRejectsMalformedHTTPStatusLines(t *testing.T) {
	for _, tc := range []struct {
		name string
		xml  string
	}{
		{
			name: "response status with 2xx-looking suffix",
			xml:  `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/a.vcf</D:href><D:status>garbage 200</D:status></D:response></D:multistatus>`,
		},
		{
			name: "propstat status with 2xx-looking suffix",
			xml:  `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/a.vcf</D:href><D:propstat><D:prop/><D:status>garbage 200</D:status></D:propstat></D:response></D:multistatus>`,
		},
		{
			name: "out of range status",
			xml:  `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/a.vcf</D:href><D:status>HTTP/1.1 999 Impossible</D:status></D:response></D:multistatus>`,
		},
		{
			name: "missing propstat status",
			xml:  `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/a.vcf</D:href><D:propstat><D:prop/></D:propstat></D:response></D:multistatus>`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseMultiStatus([]byte(tc.xml), DefaultXMLLimits())
			require.ErrorContains(t, err, "status")
		})
	}
}

func TestParseMultiStatusAllowsAbsentResponseStatusWhenPropstatIsValid(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const body = `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/a.vcf</D:href><D:propstat><D:prop/><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response></D:multistatus>`
	multi, err := ParseMultiStatus([]byte(body), DefaultXMLLimits())
	require.NoError(err)
	require.Len(multi.Responses, 1)
	assert.Zero(multi.Responses[0].StatusCode)
	assert.Equal(200, multi.Responses[0].PropStats[0].StatusCode)
}

func TestParseMultiStatusAcceptsHTTPStatusWithoutReasonPhrase(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const body = `<D:multistatus xmlns:D="DAV:"><D:response><D:href>/a.vcf</D:href><D:status>HTTP/1.1 200</D:status><D:propstat><D:prop/><D:status>HTTP/1.1 204</D:status></D:propstat></D:response></D:multistatus>`
	multi, err := ParseMultiStatus([]byte(body), DefaultXMLLimits())
	require.NoError(err)
	require.Len(multi.Responses, 1)
	assert.Equal(200, multi.Responses[0].StatusCode)
	require.Len(multi.Responses[0].PropStats, 1)
	assert.Equal(204, multi.Responses[0].PropStats[0].StatusCode)
}

func TestParseMultiStatusRequiresSingleDigitHTTPVersionComponents(t *testing.T) {
	require := require.New(t)

	valid := []byte(`<D:multistatus xmlns:D="DAV:"><D:response><D:href>/a.vcf</D:href><D:status>HTTP/1.1 200</D:status></D:response></D:multistatus>`)
	multi, err := ParseMultiStatus(valid, DefaultXMLLimits())
	require.NoError(err)
	require.Len(multi.Responses, 1)
	assert.Equal(t, 200, multi.Responses[0].StatusCode)

	malformed := []byte(`<D:multistatus xmlns:D="DAV:"><D:response><D:href>/a.vcf</D:href><D:status>HTTP/12.34 200</D:status></D:response></D:multistatus>`)
	_, err = ParseMultiStatus(malformed, DefaultXMLLimits())
	require.ErrorContains(err, "status")
}

func TestRequestBodiesUseDAVNamespaces(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	propfind, err := PropfindBody([]PropertyName{CurrentUserPrincipalProperty})
	require.NoError(err)
	assert.Contains(string(propfind), "DAV:")
	query, err := AddressbookQueryBody([]PropertyName{GetETagProperty})
	require.NoError(err)
	assert.Contains(string(query), "urn:ietf:params:xml:ns:carddav")

	_, err = PropfindBody([]PropertyName{{Namespace: "https://example.invalid", Local: "injected"}})
	require.ErrorIs(err, ErrInvalidProperty)
	_, err = PropfindBody([]PropertyName{{Namespace: "DAV:", Local: `getetag/><X:evil`}})
	require.ErrorIs(err, ErrInvalidProperty)
}
