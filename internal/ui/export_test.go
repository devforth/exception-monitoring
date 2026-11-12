package ui

import "time"

// Compiled only into tests: the external ui_test package uses it to keep the
// slow credentials key derivation out of every test server start.
func init() {
	credentialsIterations = 1_000
}

// SetResendCooldown changes the minimum time between two verification codes
// to one address and returns a function that restores it.
func SetResendCooldown(cooldown time.Duration) (restore func()) {
	previous := resendCooldown
	resendCooldown = cooldown

	return func() { resendCooldown = previous }
}
