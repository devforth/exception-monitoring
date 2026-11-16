package contracts

import "github.com/devforth/exception-monitoring/internal/platform/config"

// Limits of UI fields that config does not hold.
const (
	MaxConnectionName      = 100 // the name of a notification connection
	VerificationCodeDigits = 6   // an email confirmation code
)

// FieldLimits are the limits of form fields as templates use them (function
// "limits", for maxlength, minlength and pattern), so the forms and the
// server's checks never differ.
type FieldLimits struct {
	ConnectionName, Username, UserName, MinPassword, ProjectName, VerificationCode int
	UsernamePattern, ProjectNamePattern                                            string
}

var Limits = FieldLimits{
	ConnectionName: MaxConnectionName, Username: config.MaxUsername, UserName: config.MaxUserName, MinPassword: config.MinPasswordLength,
	ProjectName: config.MaxProjectName, VerificationCode: VerificationCodeDigits,
	UsernamePattern: config.UsernamePattern, ProjectNamePattern: config.ProjectNamePattern,
}
