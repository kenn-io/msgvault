package mime

import (
	"encoding/csv"
	"strconv"
	"strings"
)

// GoogleGroupsHeaders holds the metadata accompanying messages in Groups
// Takeout MBOX files. Group is empty for mail without Groups headers unless
// fallbackGroup explicitly identifies the archive as a Groups export.
type GoogleGroupsHeaders struct {
	Group    string
	ThreadID string
	Labels   []string
}

// ParseGoogleGroupsHeaders reads only the bounded header block, including when
// a malformed MIME body would prevent full parsing. fallbackGroup is the
// caller's group identifier for an explicitly selected Google Groups source.
func ParseGoogleGroupsHeaders(raw []byte, fallbackGroup string) GoogleGroupsHeaders {
	headers := tokenizeHeaders(raw)
	group := strings.TrimSpace(firstHeader(headers, "x-google-groups"))
	if group == "" {
		address := strings.TrimSpace(firstHeader(headers, "x-beenthere"))
		local, domain, ok := strings.Cut(address, "@")
		if ok && local != "" && strings.EqualFold(domain, "googlegroups.com") {
			group = local
		}
	}
	if group == "" {
		group = strings.TrimSpace(fallbackGroup)
	}
	if group == "" {
		return GoogleGroupsHeaders{}
	}
	// Bare group names and public addresses share a case-insensitive identity.
	if local, domain, ok := strings.Cut(group, "@"); !ok {
		group = strings.ToLower(group)
	} else if local != "" && strings.EqualFold(domain, "googlegroups.com") {
		group = strings.ToLower(local)
	}
	result := GoogleGroupsHeaders{Group: strings.ToValidUTF8(group, "\uFFFD")}
	thread := strings.TrimSpace(firstHeader(headers, "x-gm-thrid"))
	if id, err := strconv.ParseUint(thread, 10, 64); err == nil && id != 0 {
		result.ThreadID = strconv.FormatUint(id, 10)
	}
	result.Labels = append(result.Labels, result.Group)
	for _, value := range headers["x-gmail-labels"] {
		reader := csv.NewReader(strings.NewReader(value))
		reader.TrimLeadingSpace = true
		labels, err := reader.Read()
		if err != nil {
			labels = strings.Split(value, ",")
		}
		for _, label := range labels {
			if label = strings.TrimSpace(strings.ToValidUTF8(decodeHeader(label), "\uFFFD")); label != "" {
				result.Labels = append(result.Labels, label)
			}
		}
	}
	return result
}
