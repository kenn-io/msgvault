package activity

// The spine resolves IANA zone names in Go (not only in DuckDB's bundled ICU),
// so a release binary running on a minimal image without /usr/share/zoneinfo
// must still date events correctly. The embedded database is only a fallback:
// host zoneinfo still wins when present, so behavior does not change on
// developer machines. This is a standard-library import and adds no module
// dependency.
import (
	// Embed IANA timezone data for hosts without a system zoneinfo database.
	_ "time/tzdata"
)
