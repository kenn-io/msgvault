package vcardmap

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOrgComponentsFollowsRFC6350Ordering(t *testing.T) {
	tests := []struct {
		name       string
		employment Employment
		want       []string
	}{
		{
			name:       "organization only",
			employment: Employment{OrganizationName: "Example Org"},
			want:       []string{"Example Org"},
		},
		{
			name: "organization and one unit",
			employment: Employment{
				OrganizationName: "Example Org",
				Department:       "Archive Platform",
			},
			want: []string{"Example Org", "Archive Platform"},
		},
		{
			name: "nested units split on the unit separator",
			employment: Employment{
				OrganizationName: "Example Org",
				Department:       "Engineering / Archive Platform",
			},
			want: []string{"Example Org", "Engineering", "Archive Platform"},
		},
		{
			name: "surrounding whitespace is trimmed per component",
			employment: Employment{
				OrganizationName: "  Example Org  ",
				Department:       "  Archive Platform  ",
			},
			want: []string{"Example Org", "Archive Platform"},
		},
		{
			name: "a blank department contributes no unit",
			employment: Employment{
				OrganizationName: "Example Org",
				Department:       "   ",
			},
			want: []string{"Example Org"},
		},
		{
			name:       "no organization name yields no ORG value",
			employment: Employment{Department: "Archive Platform"},
			want:       nil,
		},
		{
			name:       "empty employment yields no ORG value",
			employment: Employment{},
			want:       nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			assert.Equal(test.want, OrgComponents(test.employment))
		})
	}
}

func TestTitleAndRoleTrimAndPassThroughUnescaped(t *testing.T) {
	assert := assert.New(t)
	employment := Employment{
		OrganizationName: "Example Org",
		Title:            "  Staff Engineer  ",
		Role:             "  Engineering; Archives  ",
	}
	assert.Equal("Staff Engineer", Title(employment))
	assert.Equal("Engineering; Archives", Role(employment),
		"values stay unescaped; the vCard codec owns delimiter escaping")

	assert.Empty(Title(Employment{OrganizationName: "Example Org"}))
	assert.Empty(Role(Employment{OrganizationName: "Example Org"}))
}
