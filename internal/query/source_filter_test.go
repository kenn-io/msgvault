package query

import (
	"testing"
)

func TestAppendSourceFilter(t *testing.T) {
	id42 := int64(42)

	tests := []struct {
		name           string
		singleID       *int64
		multiIDs       []int64
		prefix         string
		wantConditions int
		wantArgs       int
		wantCondition  string
	}{
		{
			name:           "neither single nor multi",
			singleID:       nil,
			multiIDs:       nil,
			prefix:         "m.",
			wantConditions: 0,
			wantArgs:       0,
		},
		{
			name:           "single ID",
			singleID:       &id42,
			multiIDs:       nil,
			prefix:         "m.",
			wantConditions: 1,
			wantArgs:       1,
			wantCondition:  "m.source_id = ?",
		},
		{
			name:           "empty multi IDs matches nothing",
			singleID:       nil,
			multiIDs:       []int64{},
			prefix:         "m.",
			wantConditions: 1,
			wantArgs:       0,
			wantCondition:  "1=0",
		},
		{
			name:           "empty multi IDs overrides singleID",
			singleID:       &id42,
			multiIDs:       []int64{},
			prefix:         "m.",
			wantConditions: 1,
			wantArgs:       0,
			wantCondition:  "1=0",
		},
		{
			name:           "single multi ID",
			singleID:       nil,
			multiIDs:       []int64{7},
			prefix:         "m.",
			wantConditions: 1,
			wantArgs:       1,
			wantCondition:  "m.source_id IN (?)",
		},
		{
			name:           "multi IDs",
			singleID:       nil,
			multiIDs:       []int64{1, 2, 3},
			prefix:         "msg.",
			wantConditions: 1,
			wantArgs:       3,
			wantCondition:  "msg.source_id IN (?,?,?)",
		},
		{
			name:           "multi IDs take precedence over single",
			singleID:       &id42,
			multiIDs:       []int64{10, 20},
			prefix:         "",
			wantConditions: 1,
			wantArgs:       2,
			wantCondition:  "source_id IN (?,?)",
		},
		{
			name:           "empty prefix",
			singleID:       &id42,
			multiIDs:       nil,
			prefix:         "",
			wantConditions: 1,
			wantArgs:       1,
			wantCondition:  "source_id = ?",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conditions, args := appendSourceFilter(
				nil, nil, tt.prefix, tt.singleID, tt.multiIDs,
			)
			if len(conditions) != tt.wantConditions {
				t.Errorf("conditions = %d, want %d: %v",
					len(conditions), tt.wantConditions, conditions)
			}
			if len(args) != tt.wantArgs {
				t.Errorf("args = %d, want %d", len(args), tt.wantArgs)
			}
			if tt.wantCondition != "" && len(conditions) > 0 {
				if conditions[0] != tt.wantCondition {
					t.Errorf("condition = %q, want %q",
						conditions[0], tt.wantCondition)
				}
			}
		})
	}
}
