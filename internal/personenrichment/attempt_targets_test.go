package personenrichment_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/personfacts"
)

func TestDurableAttemptTargetsCanonicalRoundTrip(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	targets := exaDeepTargets(t)
	targets[0], targets[1] = targets[1], targets[0]

	encoded, canonical, err := personenrichment.EncodeDurableAttemptTargets(targets)
	requirements.NoError(err)
	requirements.JSONEq(`[{"kind":"attribute","key":"attribute:summary","revision":"`+
		canonical[0].Revision+`","universal_id":"attribute:summary","slug":"summary",`+
		`"description":"One-sentence public profile summary","value_type":"text",`+
		`"cardinality":"single","max_length":240,"choices":null,"fields":null,"sensitive":false},`+
		`{"kind":"employment","key":"system:employment","revision":"`+canonical[1].Revision+`",`+
		`"universal_id":"system:employment","slug":"employment",`+
		`"description":"Current and historical employment, including organization, title, role, department, location, and partial start and end dates",`+
		`"value_type":"employment","cardinality":"multi","choices":null,"fields":[`+
		`{"name":"organization","value_type":"organization","cardinality":"single","required":true},`+
		`{"name":"title","value_type":"text","cardinality":"single","required":false},`+
		`{"name":"role","value_type":"text","cardinality":"single","required":false},`+
		`{"name":"department","value_type":"text","cardinality":"single","required":false},`+
		`{"name":"location","value_type":"text","cardinality":"single","required":false},`+
		`{"name":"start_date","value_type":"partial-date","cardinality":"single","required":false},`+
		`{"name":"end_date","value_type":"partial-date","cardinality":"single","required":false}],"sensitive":false}]`, encoded)
	checks.Equal("attribute:summary", canonical[0].Key)
	targets[1].Description = "mutated"
	checks.NotEqual(targets[1].Description, canonical[0].Description)

	decoded, err := personenrichment.DecodeDurableAttemptTargets(encoded)
	requirements.NoError(err)
	checks.Equal(canonical, decoded)
	canonical[1].Fields[0].Name = "mutated"
	checks.NotEqual(canonical[1].Fields[0].Name, decoded[1].Fields[0].Name)
}

func TestDurableAttemptTargetsRejectUnsafeOrNonCanonicalEncoding(t *testing.T) {
	valid := exaDeepTargets(t)
	encoded, _, err := personenrichment.EncodeDurableAttemptTargets(valid)
	require.NoError(t, err)

	stale := append([]personfacts.TargetDescriptor(nil), valid...)
	stale[0].Revision = strings.Repeat("0", 64)
	duplicate := []personfacts.TargetDescriptor{valid[0], valid[0]}
	tooMany := make([]personfacts.TargetDescriptor, 101)
	for i := range tooMany {
		tooMany[i] = valid[0]
		tooMany[i].Key = "attribute:" + strings.Repeat("x", i+1)
		tooMany[i].UniversalID = tooMany[i].Key
		tooMany[i].Slug = strings.Repeat("x", i+1)
		tooMany[i].Revision = exaTargetRevision(t, tooMany[i])
	}
	oversized := append([]personfacts.TargetDescriptor(nil), valid...)
	oversized[0].Description = strings.Repeat("x", 256<<10)
	oversized[0].Revision = exaTargetRevision(t, oversized[0])
	invalidKind := append([]personfacts.TargetDescriptor(nil), valid...)
	invalidKind[0].Kind = personfacts.TargetKind("invented")
	invalidKind[0].Revision = exaTargetRevision(t, invalidKind[0])

	for name, targets := range map[string][]personfacts.TargetDescriptor{
		"empty": nil, "stale revision": stale, "duplicate": duplicate,
		"too many": tooMany, "oversized": oversized, "invalid kind": invalidKind,
	} {
		t.Run("encode "+name, func(t *testing.T) {
			_, _, encodeErr := personenrichment.EncodeDurableAttemptTargets(targets)
			assert.Error(t, encodeErr)
		})
	}

	for name, raw := range map[string]string{
		"empty":              "",
		"null":               "null",
		"trailing":           encoded + `{}`,
		"noncanonical space": " " + encoded,
		"unknown":            strings.Replace(encoded, `"kind":`, `"extra":false,"kind":`, 1),
		"case variant":       strings.Replace(encoded, `"kind":`, `"Kind":`, 1),
		"duplicate member":   strings.Replace(encoded, `"kind":`, `"kind":"attribute","kind":`, 1),
		"oversized":          `[` + strings.Repeat(" ", 256<<10) + `]`,
	} {
		t.Run("decode "+name, func(t *testing.T) {
			_, decodeErr := personenrichment.DecodeDurableAttemptTargets(raw)
			assert.Error(t, decodeErr)
		})
	}
}
