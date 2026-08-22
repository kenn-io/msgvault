package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCardDAVSchedulerJobNameIsStable(t *testing.T) {
	assert.Equal(t, "carddav", CardDAVJobName)
}
