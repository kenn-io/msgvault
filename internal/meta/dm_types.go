package meta

const (
	SourceTypeFacebookMessages  = "facebook_messages"
	SourceTypeInstagramMessages = "instagram_messages"
	dmRawFormat                 = "meta-dm-json"
	dmMetadataVersion           = 1
	dmBackfillCap               = 20
)

type DMPlatform string

const (
	PlatformMessenger DMPlatform = "messenger"
	PlatformInstagram DMPlatform = "instagram"
)

func (p DMPlatform) SourceType() string {
	switch p {
	case PlatformMessenger:
		return SourceTypeFacebookMessages
	case PlatformInstagram:
		return SourceTypeInstagramMessages
	}
	return ""
}
func (p DMPlatform) GraphPlatform() string { return string(p) }

// DMSource binds an inbox to its app and verified Page or linked IG account.
type DMSource struct {
	AppID, PageID, IGID string
	Platform            DMPlatform
}

func (s DMSource) Identifier() string {
	if s.Platform == PlatformInstagram {
		return InstagramSourceIdentifier(s.AppID, s.PageID, s.IGID)
	}
	return SourceIdentifier(s.AppID, s.PageID)
}
func ParseDMSourceIdentifier(p DMPlatform, identifier string) (DMSource, bool) {
	s := DMSource{Platform: p}
	if p == PlatformInstagram {
		id, ok := ParseInstagramSourceIdentifier(identifier)
		s.AppID, s.PageID, s.IGID = id.AppID, id.PageID, id.AccountID
		return s, ok
	}
	if p != PlatformMessenger {
		return DMSource{}, false
	}
	var ok bool
	s.AppID, s.PageID, ok = ParseSourceIdentifier(identifier)
	return s, ok
}
