package ui

import (
	"time"

	"github.com/devforth/exception-monitoring/internal/ui/services"
)

// Compiled only into tests: the external ui_test package uses it to keep the
// slow credentials key derivation out of every test server start.
func init() {
	services.CredentialsIterations = 1_000
}

// SetResendCooldown changes the minimum time between two verification codes
// to one address and returns a function that restores it.
func SetResendCooldown(cooldown time.Duration) (restore func()) {
	previous := services.ResendCooldown
	services.ResendCooldown = cooldown

	return func() { services.ResendCooldown = previous }
}
