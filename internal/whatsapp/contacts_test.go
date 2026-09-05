package whatsapp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportContactsPhoneCountryCodePolicy(t *testing.T) {
	tests := []struct {
		name             string
		tel              string
		participantPhone string
		wantMatched      int
		wantDisplayName  string
	}{
		{
			name:             "bare ten digit issue reproduction",
			tel:              "TEL:2025551234",
			participantPhone: "+12025551234",
			wantMatched:      0,
			wantDisplayName:  "",
		},
		{
			name:             "explicit plus",
			tel:              "TEL:+1-202-555-0101",
			participantPhone: "+12025550101",
			wantMatched:      1,
			wantDisplayName:  "Test User",
		},
		{
			name:             "explicit zero zero",
			tel:              "TEL:00 1 202-555-0102",
			participantPhone: "+12025550102",
			wantMatched:      1,
			wantDisplayName:  "Test User",
		},
		{
			name:             "phone context",
			tel:              "TEL;VALUE=uri:tel:555-0103;phone-context=+1-202",
			participantPhone: "+12025550103",
			wantMatched:      1,
			wantDisplayName:  "Test User",
		},
		{
			name:             "local trunk",
			tel:              "TEL:077-555-0100",
			participantPhone: "+10775550100",
			wantMatched:      0,
			wantDisplayName:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			participantID, err := st.EnsureParticipantByPhone(tt.participantPhone, "", "whatsapp")
			require.NoError(err)

			path := filepath.Join(t.TempDir(), "contacts.vcf")
			vcf := "BEGIN:VCARD\r\n" +
				"VERSION:3.0\r\n" +
				"FN:Test User\r\n" +
				tt.tel + "\r\n" +
				"END:VCARD\r\n"
			require.NoError(os.WriteFile(path, []byte(vcf), 0o600))

			matched, total, err := ImportContacts(st, path)
			require.NoError(err)
			assert.Equal(1, total)
			assert.Equal(tt.wantMatched, matched)

			displayNames, err := st.ParticipantDisplayNamesContext(t.Context(), []int64{participantID})
			require.NoError(err)
			if tt.wantDisplayName == "" {
				assert.Equal(map[int64]string{}, displayNames)
			} else {
				assert.Equal(map[int64]string{participantID: tt.wantDisplayName}, displayNames)
			}
		})
	}
}
