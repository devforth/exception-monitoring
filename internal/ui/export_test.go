package ui

// Compiled only into tests: the external ui_test package uses it to keep the
// slow credentials key derivation out of every test server start.
func init() {
	credentialsIterations = 1_000
}
