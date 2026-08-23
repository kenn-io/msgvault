package carddav

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/vcard"
)

func TestPublicationVersionUsesV4OnlyWhenItIsTheOnlyAdvertisedVersion(t *testing.T) {
	assert.Equal(t, vcard.Version30, publicationVersion(nil))
	assert.Equal(t, vcard.Version30, publicationVersion([]string{"3.0", "4.0"}))
	assert.Equal(t, vcard.Version40, publicationVersion([]string{"4.0"}))
}

func TestPublicationHrefBuildsEscapedDirectCollectionChild(t *testing.T) {
	assert := assert.New(t)

	origin, err := url.Parse("https://contacts.example")
	require.NoError(t, err)
	service := &Service{client: &Client{origin: originURL(origin)}}

	href, err := service.publicationHref(
		"https://contacts.example/books/personal?ignored=yes#fragment", "a/b?c#d",
	)
	require.NoError(t, err)
	assert.Equal("https://contacts.example:443/books/personal/a%2Fb%3Fc%23d.vcf", href)

	_, err = service.publicationHref("https://elsewhere.example/books/personal/", "person")
	assert.ErrorIs(err, ErrUnsafeTarget)
}

func TestPublicationHrefUsesCanonicalDefaultHTTPSPort(t *testing.T) {
	origin, err := url.Parse("https://contacts.example")
	require.NoError(t, err)
	service := &Service{client: &Client{origin: originURL(origin)}}

	href, err := service.publicationHref("https://CONTACTS.example/books/personal/", "alice")

	require.NoError(t, err)
	assert.Equal(t, "https://contacts.example:443/books/personal/alice.vcf", href)
}

func TestStripServerOwnedPropertiesRemovesOnlyNamedProperties(t *testing.T) {
	assert := assert.New(t)

	body := []byte("BEGIN:VCARD\r\nVERSION:3.0\r\nUID:test\r\nFN:Test\r\nPRODID:server\r\nREV:one\r\nSOURCE:https://example.test\r\nCREATED:one\r\nLAST-MODIFIED:two\r\nX-KEEP:yes\r\nEND:VCARD\r\n")

	got, err := stripServerOwnedProperties(body, vcard.Version30)
	require.NoError(t, err)
	assert.NotContains(string(got), "PRODID:")
	assert.NotContains(string(got), "LAST-MODIFIED:")
	assert.Contains(string(got), "X-KEEP:yes")
}
