package threads

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"slices"
)

func highUsage(h http.Header) bool {
	for _, header := range []string{"X-App-Usage", "X-Page-Usage"} {
		var app map[string]jsontext.Value
		if json.Unmarshal([]byte(h.Get(header)), &app) == nil && usageMetrics(app) {
			return true
		}
	}
	var business map[string][]map[string]jsontext.Value
	if json.Unmarshal([]byte(h.Get("X-Business-Use-Case-Usage")), &business) == nil {
		for _, uses := range business {
			if slices.ContainsFunc(uses, usageMetrics) {
				return true
			}
		}
	}
	return false
}

func usageMetrics(metrics map[string]jsontext.Value) bool {
	for _, key := range []string{"call_count", "total_cputime", "total_time"} {
		var percentage float64
		if value, ok := metrics[key]; ok && json.Unmarshal(value, &percentage) == nil && percentage >= 80 {
			return true
		}
	}
	return false
}
