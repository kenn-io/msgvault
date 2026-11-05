package testutil

// DAVMultiStatus wraps test responses in a DAV envelope with the provider's namespace.
func DAVMultiStatus(responses, namespace string) string {
	return `<D:multistatus xmlns:D="DAV:" xmlns:C="` + namespace + `">` + responses + `</D:multistatus>`
}

// DAVPropResponse wraps properties in a successful DAV response.
func DAVPropResponse(href, props string) string {
	return `<D:response><D:href>` + href + `</D:href><D:propstat><D:prop>` + props + `</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`
}
