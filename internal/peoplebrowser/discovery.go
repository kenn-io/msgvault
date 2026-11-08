package peoplebrowser

import "context"

// SemanticPeopleSearcher uses the daemon's separately consented person corpus.
// Supporting the transport does not imply runtime enablement or index readiness.
type SemanticPeopleSearcher interface {
	SearchSemanticPeople(ctx context.Context, request SemanticPeopleRequest) (*SemanticPeoplePage, error)
}

type SemanticPeopleRequest struct {
	Query          string
	Limit          int
	OrganizationID int64
}

type SemanticPersonMatch struct {
	PersonID    int64   `json:"person_id"`
	DisplayName *string `json:"display_name"`
	VCardUID    string  `json:"vcard_uid"`
	Score       float64 `json:"score"`
}

type SemanticPeoplePage struct {
	Results []SemanticPersonMatch `json:"results"`
}

// OrganizationLister resolves canonical names and current saved aliases without
// choosing between ambiguous organizations or reading private profile content.
type OrganizationLister interface {
	ListOrganizations(ctx context.Context, query string, limit, offset int) (*OrganizationPage, error)
}

type OrganizationSummary struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type OrganizationPage struct {
	Organizations []OrganizationSummary `json:"organizations"`
	Total         int64                 `json:"total"`
	Limit         int                   `json:"limit"`
	Offset        int                   `json:"offset"`
	HasMore       bool                  `json:"has_more"`
}
