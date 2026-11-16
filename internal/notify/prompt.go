package notify

import (
	"fmt"
	"strings"

	"github.com/devforth/exception-monitoring/internal/agentprompt"
)

// agentPrompt is the prompt for a coding agent that every channel sends with
// an error: a thread reply in Slack, a file in Telegram and email.
func agentPrompt(notification Notification) string {
	return agentprompt.Build(agentprompt.Input{
		Project: notification.Project, Title: notification.Title, Level: notification.Level, Culprit: notification.Culprit,
		Kind: notification.Kind.label(), Event: notification.Event,
	})
}

// promptCaption introduces the prompt in its Slack thread.
const promptCaption = "Prompt for a coding agent: it reproduces and explains the error without changing code."

// promptFileName names the prompt file after the issue, such as
// "backend-error-6.md", so prompts saved from several messages do not
// overwrite each other.
func promptFileName(notification Notification) string {
	level := notification.Level

	if level == "" {
		level = "error"
	}

	return fmt.Sprintf("%s-%s-%d.md", fileNamePart(notification.Project), fileNamePart(level), notification.IssueID)
}

// fileNamePart keeps letters, digits and dashes, lowercase: project names
// come from config and may hold anything.
func fileNamePart(text string) string {
	var part strings.Builder

	for _, char := range strings.ToLower(text) {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9', char == '-':
			part.WriteRune(char)
		case part.Len() > 0 && !strings.HasSuffix(part.String(), "-"):
			part.WriteRune('-')
		}
	}

	if name := strings.Trim(part.String(), "-"); name != "" {
		return name
	}

	return "project"
}
