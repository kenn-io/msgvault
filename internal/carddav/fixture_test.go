package carddav

const cardDAVMultiStatusFixture = `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:" xmlns:CR="urn:ietf:params:xml:ns:carddav">
  <D:response>
    <D:href>/dav/books/personal/alice.vcf</D:href>
    <D:propstat>
      <D:prop>
        <D:getetag>"abc"</D:getetag>
        <CR:address-data>BEGIN:VCARD
VERSION:4.0
END:VCARD</CR:address-data>
      </D:prop>
      <D:status>HTTP/1.1 200 OK</D:status>
    </D:propstat>
  </D:response>
</D:multistatus>`
