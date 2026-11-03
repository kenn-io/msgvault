package meta

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassifyErrorTable(t *testing.T) {
	for _, tt := range []struct {
		code, subcode int
		want          ErrorClass
	}{
		{4, 0, ClassRateLimited}, {17, 0, ClassRateLimited}, {32, 0, ClassRateLimited}, {613, 0, ClassRateLimited},
		{613, 1893016, ClassRateLimited}, {613, 2018338, ClassRateLimited}, {613, 2534040, ClassRateLimited},
		{80000, 0, ClassRateLimited}, {80001, 0, ClassRateLimited}, {80002, 0, ClassRateLimited}, {80006, 0, ClassRateLimited}, {80014, 0, ClassRateLimited},
		{190, 0, ClassTokenInvalid}, {200, 2018028, ClassAppAccess}, {200, 0, ClassPermissionMissing},
		{10, 0, ClassPermissionMissing}, {3, 0, ClassPermissionMissing}, {283, 0, ClassPermissionMissing}, {299, 0, ClassPermissionMissing},
		{10, 2534022, ClassWindowClosed}, {10, 2018278, ClassWindowClosed}, {10, 2018065, ClassWindowClosed},
		{3801, 0, ClassRejected}, {3802, 0, ClassRejected}, {368, 0, ClassRejected},
		{100, 2534025, ClassPrivateReplyNotAllowed}, {10, 2018300, ClassThreadOwned}, {100, 2534037, ClassThreadOwned},
		{551, 0, ClassRecipientUnavailable}, {551, 1545041, ClassRecipientUnavailable}, {10, 2018108, ClassRecipientUnavailable},
		{100, 0, ClassRejected}, {999, 0, ClassRejected},
	} {
		t.Run(fmt.Sprintf("%d/%d", tt.code, tt.subcode), func(t *testing.T) {
			e := fmt.Errorf("wrapped: %w", &APIError{Status: 400, Code: tt.code, Subcode: tt.subcode})
			assert.Equal(t, tt.want, Classify(e, true))
			assert.Equal(t, tt.code == 613 && tt.subcode == 2018338, AbuseWarning(e))
		})
	}
	assert := assert.New(t)
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, fmt.Errorf("wrapped: %w", context.Canceled)} {
		assert.Equal(ClassUnknown, Classify(err, true))
		assert.Equal(ClassCancelled, Classify(err, false))
	}
	assert.Equal(ClassUnknown, Classify(errors.New("transport failed"), true))
	assert.Equal(ClassUnknown, Classify(&APIError{Status: 503, Code: 200}, true))
	assert.False(AbuseWarning(nil))
	assert.False(AbuseWarning(errors.New("transport failed")))
}
