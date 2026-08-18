package sponsor

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsAuthorizedBypass(t *testing.T) {
	t.Setenv("EVCC_DO_NOT_SET", "1")
	assert.True(t, IsAuthorized())
	assert.True(t, IsAuthorizedForApi())
}

func TestIsAuthorizedNoBypass(t *testing.T) {
	t.Setenv("EVCC_DO_NOT_SET", "")

	mu.Lock()
	Subject = ""
	Token = ""
	mu.Unlock()

	assert.False(t, IsAuthorized())
	assert.False(t, IsAuthorizedForApi())
}

func TestIsAuthorizedWithSubject(t *testing.T) {
	t.Setenv("EVCC_DO_NOT_SET", "")

	mu.Lock()
	Subject = "test@example.com"
	Token = "some-token"
	mu.Unlock()

	assert.True(t, IsAuthorized())
	assert.True(t, IsAuthorizedForApi())

	mu.Lock()
	Subject = ""
	Token = ""
	mu.Unlock()
}
