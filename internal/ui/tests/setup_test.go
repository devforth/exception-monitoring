package tests

import (
	"time"

	"github.com/devforth/exception-monitoring/internal/services"
)

// The slow password key derivation would take seconds per test server under
// the race detector.
func init() {
	services.PasswordIterations = 1_000
}

// setResendCooldown changes the minimum time between two verification codes
// to one address and returns a function that restores it.
func setResendCooldown(cooldown time.Duration) (restore func()) {
	previous := services.ResendCooldown
	services.ResendCooldown = cooldown

	return func() { services.ResendCooldown = previous }
}
