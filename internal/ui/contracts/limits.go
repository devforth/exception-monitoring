package contracts

import "github.com/devforth/exception-monitoring/internal/platform/config"

// FieldLimits are the limits of form fields as templates use them (function
// "limits", for maxlength, minlength and pattern), so the forms and the
// server's checks never differ.
type FieldLimits struct {
	ConnectionName, Username, UserName, MinPassword, ProjectName, VerificationCode int
	UsernamePattern, ProjectNamePattern                                            string
}

var Limits = FieldLimits{
	ConnectionName: config.MaxConnectionName, Username: config.MaxUsername, UserName: config.MaxUserName, MinPassword: config.MinPasswordLength,
	ProjectName: config.MaxProjectName, VerificationCode: config.VerificationCodeDigits,
	UsernamePattern: config.UsernamePattern, ProjectNamePattern: config.ProjectNamePattern,
}
