package api

import (
	"net/url"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/personcard"
)

func parsePersonCardOptions(q url.Values, now time.Time) (personcard.Options, string) {
	o := personcard.Options{AsOf: now.UTC(), MaxItems: 3, MaxBytes: 16384, Format: personcard.FormatJSON}
	for _, v := range []struct {
		name   string
		dst    *int
		lo, hi int
	}{{"max_items", &o.MaxItems, 1, 10}, {"max_bytes", &o.MaxBytes, 2048, 65536}} {
		if q.Has(v.name) {
			n, err := strconv.Atoi(q.Get(v.name))
			if err != nil || n < v.lo || n > v.hi {
				return o, "invalid_" + v.name
			}
			*v.dst = n
		}
	}
	if q.Has("as_of") {
		t, err := time.Parse(time.RFC3339, q.Get("as_of"))
		if err != nil || t.Before(now.AddDate(-10, 0, 0)) || t.After(now.AddDate(1, 0, 0)) {
			return o, "invalid_as_of"
		}
		o.AsOf = t.UTC()
	}
	if q.Has("format") {
		o.Format = personcard.Format(q.Get("format"))
		if o.Format != personcard.FormatJSON && o.Format != personcard.FormatMarkdown {
			return o, "invalid_format"
		}
	}
	if q.Has("affiliation") {
		o.Affiliation = q.Get("affiliation")
		if o.Affiliation != "internal" && o.Affiliation != "external" {
			return o, "invalid_affiliation"
		}
	}
	return o, ""
}
