package contracts

// LoginData is the login form.
type LoginData struct {
	Next     string
	Error    string
	Username string
	Disabled bool
}
