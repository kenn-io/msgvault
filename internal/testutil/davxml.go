package testutil

// DAVMultiStatus wraps test responses in a DAV envelope with the provider's namespace.
func DAVMultiStatus(responses, namespace string) string {
	return `<D:multistatus xmlns:D="DAV:" xmlns:C="` + namespace + `">` + responses + `</D:multistatus>`
}
