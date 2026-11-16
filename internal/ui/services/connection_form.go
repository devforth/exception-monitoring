package services

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
)

// The connection form: the UI posts it, and agents send the same fields as
// a tool's input (url.Values with the form's names), so both are checked by
// the same rules.

// SavedConnection is the outcome of Save: the connection as read from the
// form, the field errors, or the connection that already sends to the same
// place. Nothing is saved when Problems is not empty.
type SavedConnection struct {
	Connection store.Connection
	Problems   map[string]string
	Duplicate  *contracts.Duplicate
}

// Save applies the form to connection and stores it: a new one when its ID
// is 0 (the type comes from the form; an email connection needs the
// destination_id of a confirmed address), the stored one otherwise (an
// empty chat field keeps the stored token).
func (service *Connections) Save(ctx context.Context, form url.Values, connection store.Connection) (SavedConnection, error) {
	isNew := connection.ID == 0

	if isNew {
		connection = store.Connection{Type: strings.TrimSpace(form.Get("type"))}
	}

	problems := readConnectionForm(form, &connection, service.projects)
	result := SavedConnection{Connection: connection, Problems: problems}

	var chat chatSecret

	if available, reason := service.notifications.ChannelAvailable(connection.Type); isNew && !available {
		problems["type"] = "This type is not available: " + reason + "."
	}

	switch {
	case isNew && connection.Type == config.ChannelEmail:
		// The address must have been confirmed first (StartEmail, ConfirmEmail).
		destinationID, _ := strconv.ParseInt(form.Get("destination_id"), 10, 64)
		destination, err := emailDestination(ctx, service.store, destinationID)

		if err != nil || !destination.Verified {
			problems["address"] = "Confirm the email address first."
		} else {
			connection.DestinationID, connection.Address, connection.Verified = destination.ID, destination.Address, true
			chat.Key = destination.Address
		}
	case isNew:
		chat = readChatSecret(form, service.factory, connection.Type, "", len(problems) == 0)
		maps.Copy(problems, chat.Problems)
	default:
		chat = readChatSecret(form, service.factory, connection.Type, connection.Secret, len(problems) == 0)
		maps.Copy(problems, chat.Problems)

		if connection.Active && !connection.Verified {
			problems["active"] = "Confirm the email address first."
		}
	}

	result.Connection = connection

	// One connection per address or chat; a replaced token must not point
	// to a chat another connection uses.
	duplicate, err := service.FindDuplicate(ctx, connection.Type, chat.Key, connection.ID)

	if err != nil {
		return result, err
	}

	if duplicate != nil {
		problems["duplicate"] = "already added"
		result.Duplicate = duplicate
	}

	if len(problems) > 0 {
		return result, nil
	}

	if connection.Name == "" {
		connection.Name = service.DefaultName(ctx, connection, cmp.Or(chat.Secret, connection.Secret))
	}

	if isNew {
		connection.ID, err = service.create(ctx, connection, chat.Secret)
	} else {
		err = service.update(ctx, connection, chat.Secret)
	}

	result.Connection = connection

	return result, err
}

// TestChat sends a test message with the chat settings of the form before
// they are saved. existing is the stored connection, if any: its empty
// fields keep the stored tokens.
func (service *Connections) TestChat(ctx context.Context, form url.Values, channelType string, existing store.Connection) contracts.ChatTest {
	if channelType != config.ChannelTelegram && channelType != config.ChannelSlack {
		return contracts.ChatTest{Message: "Choose Telegram or Slack."}
	}

	secret := readChatSecret(form, service.factory, channelType, existing.Secret, true)

	if len(secret.Problems) > 0 {
		problems := slices.Sorted(maps.Values(secret.Problems))

		return contracts.ChatTest{Message: strings.Join(problems, " ")}
	}

	name := cmp.Or(strings.TrimSpace(form.Get("name")), existing.Name, "new connection")
	connection := store.Connection{Type: channelType, Name: name, Secret: secret.Secret, HideIssueLink: form.Get("include_issue_link") != "on"}

	if err := service.sendTest(ctx, connection); err != nil {
		return contracts.ChatTest{Message: "Not delivered: " + err.Error()}
	}

	return contracts.ChatTest{Sent: true, Message: "Test message sent: check the chat."}
}

// SendTest sends a test message to a saved connection. An unconfirmed
// address gets nothing beyond its code (store.ErrNotVerified).
func (service *Connections) SendTest(ctx context.Context, connection store.Connection) error {
	if !connection.Verified {
		return store.ErrNotVerified
	}

	return service.sendTest(ctx, connection)
}

func (service *Connections) sendTest(ctx context.Context, connection store.Connection) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)

	defer cancel()

	return service.factory.SendTest(ctx, connection, service.notifications.PublicURL)
}

var (
	telegramTokenRe = regexp.MustCompile(`^\d+:[A-Za-z0-9_-]{20,}$`)
	telegramChatRe  = regexp.MustCompile(`^(-?\d+|@[A-Za-z0-9_]{5,})$`)
	slackTokenRe    = regexp.MustCompile(`^xoxb-[A-Za-z0-9-]{10,}$`)
	slackChannelRe  = regexp.MustCompile(`^#?[A-Za-z0-9._-]{1,80}$`)
)

// readConnectionForm applies the rule fields (name, projects, levels,
// active) to connection and returns the field errors.
func readConnectionForm(form url.Values, connection *store.Connection, projectList config.ProjectList) map[string]string {
	problems := map[string]string{}

	// An empty name is filled in on save (see services.Connections.DefaultName).
	connection.Name = strings.TrimSpace(form.Get("name"))

	if len([]rune(connection.Name)) > contracts.MaxConnectionName {
		problems["name"] = fmt.Sprintf("Enter a name of up to %d characters, or leave it empty.", contracts.MaxConnectionName)
	}

	connection.AllProjects = form.Get("all_projects") == "on"
	connection.ProjectIDs = nil

	if !connection.AllProjects {
		for _, value := range form["project"] {
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
		if slices.Contains(form["level"], level) {
			connection.Levels = append(connection.Levels, level)
		}
	}

	if len(connection.Levels) == 0 {
		problems["levels"] = "Select at least one level."
	}

	connection.Active = form.Get("active") == "on"

	connection.HideIssueLink = form.Get("include_issue_link") != "on"

	return problems
}

// chatSecret is what readChatSecret read from the form.
type chatSecret struct {
	// Secret is the encrypted tokens, to store; empty when the form has problems.
	Secret string
	// Key is the chat's identity (what services.Connections compares to find
	// duplicates); empty when the form has problems.
	Key string
	// Problems are the field errors of the chat fields.
	Problems map[string]string
}

// readChatSecret reads the tokens of a chat. existing is the stored secret:
// an empty field keeps its value. formValid tells whether the rest of the
// form is valid: only then is the secret encrypted and its key given, since
// a form with problems is shown again without being saved or compared.
func readChatSecret(form url.Values, factory notify.Factory, channelType, existing string, formValid bool) chatSecret {
	var (
		tokens any
		key    string
	)

	result := chatSecret{Problems: map[string]string{}}

	switch channelType {
	case config.ChannelTelegram:
		var telegram notify.TelegramSecret

		if existing != "" {
			_ = factory.DecryptSecret(existing, &telegram)
		}

		if value := strings.TrimSpace(form.Get("bot_token")); value != "" {
			telegram.BotToken = value
		}

		if value := strings.TrimSpace(form.Get("chat_id")); value != "" {
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
			if value := strings.TrimSpace(form.Get(field)); value != "" {
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

// FormValues is connection as its form would post it unchanged (chat
// fields empty: the stored tokens are kept), for a tool that changes some
// fields.
func (service *Connections) FormValues(connection store.Connection) url.Values {
	form := url.Values{"name": {connection.Name}, "level": append([]string{}, connection.Levels...)}

	checkbox := func(name string, checked bool) {
		if checked {
			form.Set(name, "on")
		}
	}

	checkbox("all_projects", connection.AllProjects)
	checkbox("active", connection.Active)
	checkbox("include_issue_link", !connection.HideIssueLink)

	for _, id := range connection.ProjectIDs {
		form.Add("project", strconv.Itoa(id))
	}

	return form
}
