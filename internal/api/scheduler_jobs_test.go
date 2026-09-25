package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCardDAVSchedulerJobNameIsStable(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "carddav", CardDAVJobName)
}
