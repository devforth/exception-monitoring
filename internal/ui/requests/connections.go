package requests

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
)

var (
	telegramTokenRe = regexp.MustCompile(`^\d+:[A-Za-z0-9_-]{20,}$`)
	telegramChatRe  = regexp.MustCompile(`^(-?\d+|@[A-Za-z0-9_]{5,})$`)
	slackTokenRe    = regexp.MustCompile(`^xoxb-[A-Za-z0-9-]{10,}$`)
	slackChannelRe  = regexp.MustCompile(`^#?[A-Za-z0-9._-]{1,80}$`)
)

// ReadConnectionForm applies the rule fields (name, projects, levels,
// active) to connection and returns the field errors.
func ReadConnectionForm(r *http.Request, connection *store.Connection, projectList config.ProjectList) map[string]string {
	problems := map[string]string{}

	// An empty name is filled in on save (see services.Connections.DefaultName).
	connection.Name = strings.TrimSpace(r.PostFormValue("name"))

	if len([]rune(connection.Name)) > contracts.MaxConnectionName {
		problems["name"] = fmt.Sprintf("Enter a name of up to %d characters, or leave it empty.", contracts.MaxConnectionName)
	}

	connection.AllProjects = r.PostFormValue("all_projects") == "on"
	connection.ProjectIDs = nil

	if !connection.AllProjects {
		for _, value := range r.PostForm["project"] {
			projectID, err := strconv.Atoi(value)

			if _, ok := projectList.List().ByID(projectID); err == nil && ok {
				connection.ProjectIDs = append(connection.ProjectIDs, projectID)
			}
		}

		if len(connection.ProjectIDs) == 0 {
			problems["projects"] = "Select at least one project, or all projects."
		}
	}

	connection.Levels = nil

	for _, level := range config.Levels {
		if slices.Contains(r.PostForm["level"], level) {
			connection.Levels = append(connection.Levels, level)
		}
	}

	if len(connection.Levels) == 0 {
		problems["levels"] = "Select at least one level."
	}

	connection.Active = r.PostFormValue("active") == "on"

	connection.HideIssueLink = r.PostFormValue("include_issue_link") != "on"

	return problems
}

// ChatSecret is what ReadChatSecret read from the form.
type ChatSecret struct {
	// Secret is the encrypted tokens, to store; empty when the form has problems.
	Secret string
	// Key is the chat's identity (what services.Connections compares to find
	// duplicates); empty when the form has problems.
	Key string
	// Problems are the field errors of the chat fields.
	Problems map[string]string
}

// ReadChatSecret reads the tokens of a chat. existing is the stored secret:
// an empty field keeps its value. formValid tells whether the rest of the
// form is valid: only then is the secret encrypted and its key given, since
// a form with problems is shown again without being saved or compared.
func ReadChatSecret(r *http.Request, factory notify.Factory, channelType, existing string, formValid bool) ChatSecret {
	var (
		tokens any
		key    string
	)

	result := ChatSecret{Problems: map[string]string{}}

	switch channelType {
	case config.ChannelTelegram:
		var telegram notify.TelegramSecret

		if existing != "" {
			_ = factory.DecryptSecret(existing, &telegram)
		}

		if value := strings.TrimSpace(r.PostFormValue("bot_token")); value != "" {
			telegram.BotToken = value
		}

		if value := strings.TrimSpace(r.PostFormValue("chat_id")); value != "" {
			telegram.ChatID = value
		}

		if !telegramTokenRe.MatchString(telegram.BotToken) {
			result.Problems["bot_token"] = "Enter the bot token from @BotFather (like 123456:ABC-...)."
		}

		if !telegramChatRe.MatchString(telegram.ChatID) {
			result.Problems["chat_id"] = "Enter the chat ID (a number, negative for groups) or @channelname."
		}

		tokens, key = telegram, telegram.ChatID
	case config.ChannelSlack:
		var slack notify.SlackSecret

		if existing != "" {
			_ = factory.DecryptSecret(existing, &slack)
		}

		// Field names differ from Telegram's: both sets are in the new form.
		for field, target := range map[string]*string{
			"webhook_url": &slack.WebhookURL, "slack_bot_token": &slack.BotToken, "slack_channel": &slack.Channel,
		} {
			if value := strings.TrimSpace(r.PostFormValue(field)); value != "" {
				*target = value
			}
		}

		// A webhook, or a bot and its channel; the bot also sends the prompt.
		switch {
		case slack.WebhookURL == "" && slack.BotToken == "" && slack.Channel == "":
			result.Problems["webhook_url"] = "Enter the incoming webhook URL, or a bot token and a channel."
		case slack.WebhookURL != "" && config.CheckHTTPURL(slack.WebhookURL) != nil:
			result.Problems["webhook_url"] = "Enter the incoming webhook URL (https://hooks.slack.com/services/...)."
		}

		if (slack.BotToken != "" || slack.Channel != "") && !slackTokenRe.MatchString(slack.BotToken) {
			result.Problems["slack_bot_token"] = "Enter the Bot User OAuth Token (xoxb-...) from OAuth & Permissions."
		}

		if slack.BotToken != "" && !slackChannelRe.MatchString(slack.Channel) {
			result.Problems["slack_channel"] = "Enter the channel ID (like C0123456789, in the channel details) the bot is invited to."
		}

		tokens, key = slack, slack.Key()
	default:
		return result
	}

	if !formValid || len(result.Problems) > 0 {
		return result
	}

	encrypted, err := factory.EncryptSecret(tokens)

	if err != nil {
		result.Problems["type"] = "Cannot store the token: " + err.Error()
	}

	result.Secret, result.Key = encrypted, key

	return result
}
