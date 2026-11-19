package catalog

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/services"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// Notification connections, as on the Notifications page. Stored tokens are
// never returned; an email address receives nothing before it is confirmed
// with the code mailed to it, which only its owner can read.
func (handler *handlers) notificationEndpoints() []Endpoint {
	return []Endpoint{
		Define(Endpoint{
			Name:        "list_connections",
			Description: "Notification connections (email, Telegram, Slack): projects, levels, whether active and confirmed, and delivery health.",
			ReadOnly:    true, Admin: true, AgentsOnly: true,
		}, handler.listConnections),
		Define(Endpoint{
			Name: "start_email_verification",
			Description: "First step of an email connection: mails a 6-digit code to the address, or says it is confirmed already. " +
				"Ask the user for the code, then call confirm_email.",
			Admin: true, AgentsOnly: true,
		}, handler.startEmailVerification),
		Define(Endpoint{
			Name:        "confirm_email",
			Description: "Confirm an email address with the code the user received (start_email_verification); then create_connection can use it.",
			Admin:       true, AgentsOnly: true,
		}, handler.confirmEmail),
		Define(Endpoint{
			Name: "create_connection",
			Description: "Add a notification connection for new and regressed issues: email (a confirmed address), telegram (bot_token, chat_id) " +
				"or slack (webhook_url, or slack_bot_token and slack_channel). Test it with send_test_message.",
			Admin: true, AgentsOnly: true, Confirm: true,
		}, handler.createConnection),
		Define(Endpoint{
			Name:        "update_connection",
			Description: "Change a connection; fields not given keep their values (tokens too). Saving an active connection retries its waiting messages.",
			Idempotent:  true, Admin: true, AgentsOnly: true, Confirm: true,
		}, handler.updateConnection),
		Define(Endpoint{
			Name:        "send_test_message",
			Description: "Send a test message through a saved connection and say whether it was delivered.",
			Admin:       true, AgentsOnly: true,
		}, handler.sendTestMessage),
		Define(Endpoint{
			Name:        "retry_connection",
			Description: "Send the messages of a connection that wait for a retry now, after the cause was fixed outside this server.",
			Idempotent:  true, Admin: true, AgentsOnly: true,
		}, handler.retryConnection),
		Define(Endpoint{
			Name:        "delete_connection",
			Description: "Delete a notification connection; its waiting messages are not sent. A confirmed address stays confirmed.",
			Destructive: true, Admin: true, AgentsOnly: true,
		}, handler.deleteConnection),
		Define(Endpoint{
			Name:        "list_deliveries",
			Description: "The delivery log, newest first: each message with its connection, issue, status (pending, sent, failed), attempts and last error.",
			ReadOnly:    true, Admin: true, AgentsOnly: true,
		}, handler.listDeliveries),
	}
}

type deliveryHealth struct {
	Pending    int        `json:"pending,omitempty"`
	Retrying   int        `json:"retrying,omitempty"`
	Failed     int        `json:"failed,omitempty"`
	LastSentAt *time.Time `json:"last_sent_at,omitempty"`
	LastError  string     `json:"last_error,omitempty"`
}

type connectionOutput struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Type             string          `json:"type"`
	Email            string          `json:"email,omitempty"`
	AllProjects      bool            `json:"all_projects"`
	Projects         []string        `json:"projects,omitempty"`
	Levels           []string        `json:"levels"`
	Active           bool            `json:"active"`
	Confirmed        bool            `json:"confirmed"`
	IncludeIssueLink bool            `json:"include_issue_link"`
	Delivery         *deliveryHealth `json:"delivery,omitempty"`
}

func (handler *handlers) connectionView(call Call, connection store.Connection, health map[string]store.DeliveryHealth) connectionOutput {
	output := connectionOutput{
		ID: connection.ID, Name: connection.Name, Type: connection.Type, AllProjects: connection.AllProjects, Levels: connection.Levels,
		Active: connection.Active, Confirmed: connection.Verified, IncludeIssueLink: !connection.HideIssueLink,
	}

	if connection.Type == config.ChannelEmail {
		output.Email = connection.Address
	}

	for _, id := range connection.ProjectIDs {
		output.Projects = append(output.Projects, handler.projectName(id))
	}

	if state, ok := health[connection.ID]; ok {
		output.Delivery = &deliveryHealth{Pending: state.Pending, Retrying: state.Retrying, Failed: state.Failed, LastError: call.Text(state.LastError)}

		if !state.LastSentAt.IsZero() {
			output.Delivery.LastSentAt = &state.LastSentAt
		}
	}

	return output
}

func (handler *handlers) listConnections(ctx context.Context, call Call, _ noInput) (Page[connectionOutput], error) {
	output := Page[connectionOutput]{Items: []connectionOutput{}}
	connections, err := handler.services.Connections.List(ctx)

	if err != nil {
		return output, err
	}

	health, err := handler.services.Connections.DeliveryHealth(ctx)

	if err != nil {
		return output, err
	}

	for _, connection := range connections {
		output.Items = append(output.Items, handler.connectionView(call, connection, health))
	}

	return output, nil
}

type emailInput struct {
	Email string `json:"email" jsonschema:"The email address."`
}

type emailStepOutput struct {
	Email     string `json:"email"`
	Confirmed bool   `json:"confirmed"`
	Message   string `json:"message"`
}

// emailStep tells the agent what the UI's email step shows.
func emailStep(step services.EmailStep) (emailStepOutput, error) {
	output := emailStepOutput{Email: step.Address, Confirmed: step.State == "verified"}

	switch {
	case step.Duplicate != nil:
		return output, Invalid("connection %s (%s) already sends to this address: update it instead", step.Duplicate.ID, step.Duplicate.Name)
	case step.Error != "":
		return output, Invalid("%s", step.Error)
	case output.Confirmed:
		output.Message = "Confirmed: create_connection can use this address."
	default:
		output.Message = "A code was mailed to the address: ask the user for it and call confirm_email."
	}

	if step.Notice != "" && !output.Confirmed {
		output.Message = step.Notice
	}

	return output, nil
}

func (handler *handlers) startEmailVerification(ctx context.Context, _ Call, input emailInput) (emailStepOutput, error) {
	step, err := handler.services.Verification.StartEmail(ctx, input.Email)

	if err != nil {
		return emailStepOutput{}, err
	}

	return emailStep(step)
}

type confirmEmailInput struct {
	Email string `json:"email" jsonschema:"The address the code was mailed to."`
	Code  string `json:"code" jsonschema:"The 6-digit code from the email."`
}

func (handler *handlers) confirmEmail(ctx context.Context, _ Call, input confirmEmailInput) (emailStepOutput, error) {
	destination, ok, err := handler.services.Verification.FindDestination(ctx, input.Email)

	if err != nil {
		return emailStepOutput{}, err
	}

	if !ok {
		return emailStepOutput{}, Invalid("no code was sent to %s: call start_email_verification first", input.Email)
	}

	step, err := handler.services.Verification.ConfirmCode(ctx, destination.ID, input.Code)

	if err != nil {
		return emailStepOutput{}, err
	}

	return emailStep(step)
}

// connectionFields are the rule fields of a connection; nil keeps the value.
type connectionFields struct {
	Name             *string   `json:"name,omitempty" jsonschema:"A name; empty names it after the address or chat."`
	Projects         *[]string `json:"projects,omitempty" jsonschema:"Project names or IDs; an empty list means every project, including future ones."`
	Levels           *[]string `json:"levels,omitempty" jsonschema:"Issue levels to notify about: fatal, error, warning, info, debug (default: the levels recorded by default)."`
	Active           *bool     `json:"active,omitempty" jsonschema:"Send notifications (default true)."`
	IncludeIssueLink *bool     `json:"include_issue_link,omitempty" jsonschema:"Link to the issue in this panel (default true for email, false for chats, which may be public)."`
}

// chatFields are the tokens of a chat; empty keeps the stored ones.
type chatFields struct {
	BotToken      string `json:"bot_token,omitempty" jsonschema:"telegram: the bot token from @BotFather."`
	ChatID        string `json:"chat_id,omitempty" jsonschema:"telegram: the chat ID (negative for groups) or @channel."`
	WebhookURL    string `json:"webhook_url,omitempty" jsonschema:"slack: an incoming webhook URL."`
	SlackBotToken string `json:"slack_bot_token,omitempty" jsonschema:"slack: a bot token (xoxb-...) instead of a webhook; the bot also posts the agent prompt in a thread."`
	SlackChannel  string `json:"slack_channel,omitempty" jsonschema:"slack: the channel ID the bot is invited to."`
}

// apply writes the given fields over the form.
func (handler *handlers) applyConnection(form url.Values, fields connectionFields, chat chatFields) error {
	if fields.Name != nil {
		form.Set("name", *fields.Name)
	}

	if fields.Projects != nil {
		form.Del("project")
		form.Del("all_projects")

		for _, nameOrID := range *fields.Projects {
			project, ok := handler.projects.List().Find(nameOrID)

			if !ok {
				return Invalid("unknown project %q (see list_projects)", nameOrID)
			}

			form.Add("project", strconv.Itoa(project.ID))
		}

		if len(*fields.Projects) == 0 {
			form.Set("all_projects", "on")
		}
	}

	if fields.Levels != nil {
		form["level"] = *fields.Levels
	}

	for name, value := range map[string]*bool{"active": fields.Active, "include_issue_link": fields.IncludeIssueLink} {
		switch {
		case value == nil:
		case *value:
			form.Set(name, "on")
		default:
			form.Del(name)
		}
	}

	for name, value := range map[string]string{
		"bot_token": chat.BotToken, "chat_id": chat.ChatID, "webhook_url": chat.WebhookURL,
		"slack_bot_token": chat.SlackBotToken, "slack_channel": chat.SlackChannel,
	} {
		if value != "" {
			form.Set(name, value)
		}
	}

	return nil
}

// connectionFormFields names the form's fields as the tools do.
var connectionFormFields = map[string]string{"address": "email", "level": "levels"}

func (handler *handlers) saveConnection(ctx context.Context, call Call, form url.Values, connection store.Connection) (connectionOutput, error) {
	saved, err := handler.services.Connections.Save(ctx, form, connection)

	if err != nil {
		return connectionOutput{}, err
	}

	if saved.Duplicate != nil {
		return connectionOutput{}, Invalid("connection %s (%s) already sends there: update it instead", saved.Duplicate.ID, saved.Duplicate.Name)
	}

	if err := problemsError(saved.Problems, connectionFormFields); err != nil {
		return connectionOutput{}, err
	}

	return handler.connectionView(call, saved.Connection, nil), nil
}

type createConnectionInput struct {
	Type  string `json:"type" jsonschema:"email, telegram or slack."`
	Email string `json:"email,omitempty" jsonschema:"email: an address confirmed with start_email_verification and confirm_email."`
	connectionFields
	chatFields
}

func (handler *handlers) createConnection(ctx context.Context, call Call, input createConnectionInput) (connectionOutput, error) {
	// A new connection starts as in the UI (store.NewConnection).
	form := handler.services.Connections.FormValues(handler.services.Connections.New(input.Type))
	form.Set("type", input.Type)

	if input.Type == config.ChannelEmail {
		destination, ok, err := handler.services.Verification.FindDestination(ctx, input.Email)

		if err != nil {
			return connectionOutput{}, err
		}

		if !ok || !destination.Verified {
			return connectionOutput{}, Invalid("email: confirm %q first with start_email_verification and confirm_email", input.Email)
		}

		form.Set("destination_id", destination.ID)
	}

	if err := handler.applyConnection(form, input.connectionFields, input.chatFields); err != nil {
		return connectionOutput{}, err
	}

	return handler.saveConnection(ctx, call, form, store.Connection{})
}

type connectionInput struct {
	ConnectionID string `json:"connection_id" jsonschema:"id from list_connections."`
}

type updateConnectionInput struct {
	ConnectionID string `json:"connection_id" jsonschema:"id from list_connections."`
	connectionFields
	chatFields
}

func (handler *handlers) updateConnection(ctx context.Context, call Call, input updateConnectionInput) (connectionOutput, error) {
	connection, err := handler.services.Connections.Get(ctx, input.ConnectionID)

	if err != nil {
		return connectionOutput{}, err
	}

	form := handler.services.Connections.FormValues(connection)

	if err := handler.applyConnection(form, input.connectionFields, input.chatFields); err != nil {
		return connectionOutput{}, err
	}

	return handler.saveConnection(ctx, call, form, connection)
}

func (handler *handlers) sendTestMessage(ctx context.Context, _ Call, input connectionInput) (doneOutput, error) {
	connection, err := handler.services.Connections.Get(ctx, input.ConnectionID)

	if err != nil {
		return doneOutput{}, err
	}

	if err := handler.services.Connections.SendTest(ctx, connection); errors.Is(err, store.ErrNotVerified) {
		return doneOutput{}, Invalid("the address is not confirmed: no test message is sent")
	} else if err != nil {
		// A refused delivery is the answer, not a server failure.
		return doneOutput{}, Invalid("not delivered: %v", err)
	}

	return doneOutput{Done: "test message sent: check the destination"}, nil
}

func (handler *handlers) retryConnection(ctx context.Context, _ Call, input connectionInput) (doneOutput, error) {
	if _, err := handler.services.Connections.Get(ctx, input.ConnectionID); err != nil {
		return doneOutput{}, err
	}

	if err := handler.services.Connections.RetryNow(ctx, input.ConnectionID); err != nil {
		return doneOutput{}, err
	}

	return doneOutput{Done: "the waiting messages are being sent now"}, nil
}

func (handler *handlers) deleteConnection(ctx context.Context, _ Call, input connectionInput) (doneOutput, error) {
	if _, err := handler.services.Connections.Get(ctx, input.ConnectionID); err != nil {
		return doneOutput{}, err
	}

	if err := handler.services.Connections.Delete(ctx, input.ConnectionID); err != nil {
		return doneOutput{}, err
	}

	return doneOutput{Done: "deleted"}, nil
}

type listDeliveriesInput struct {
	ConnectionID string `json:"connection_id,omitempty" jsonschema:"Only this connection."`
	Status       string `json:"status,omitempty" jsonschema:"pending, sent or failed."`
	Limit        int    `json:"limit,omitempty" jsonschema:"Messages per page, 1 to 100 (default 20)."`
	Cursor       string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page."`
}

type deliveryOutput struct {
	ID            int64     `json:"id"`
	ConnectionID  string    `json:"connection_id"`
	Message       string    `json:"message"`
	Project       string    `json:"project,omitempty"`
	Title         string    `json:"title,omitempty"`
	IssueID       string    `json:"issue_id,omitempty"`
	Status        string    `json:"status"`
	Attempts      int       `json:"attempts"`
	LastError     string    `json:"last_error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	NextAttemptAt time.Time `json:"next_attempt_at"`
}

func (handler *handlers) listDeliveries(ctx context.Context, call Call, input listDeliveriesInput) (Page[deliveryOutput], error) {
	output := Page[deliveryOutput]{Items: []deliveryOutput{}}

	switch input.Status {
	case "", store.OutboxPending, store.OutboxSent, store.OutboxFailed:
	default:
		return output, Invalid("status must be pending, sent or failed")
	}

	limit, err := parseLimit(input.Limit)

	if err != nil {
		return output, err
	}

	offset, err := parseCursor(input.Cursor)

	if err != nil {
		return output, err
	}

	filter := store.OutboxFilter{Status: input.Status, Limit: limit + 1, Offset: int(offset)}

	if input.ConnectionID != "" {
		filter.Channel = input.ConnectionID
	}

	records, err := handler.services.Deliveries.List(ctx, filter)

	if err != nil {
		return output, err
	}

	records, more := store.Page(records, limit)

	for _, record := range records {
		message := notify.Describe(record.Kind, record.Payload)
		output.Items = append(output.Items, deliveryOutput{
			ID: record.ID, ConnectionID: record.Channel, Message: message.Label, Project: message.Project, Title: call.Text(message.Title),
			IssueID: message.IssueID, Status: record.Status, Attempts: record.Attempts, LastError: call.Text(record.LastError),
			CreatedAt: record.CreatedAt, NextAttemptAt: record.NextAttemptAt,
		})
	}

	if more {
		output.NextCursor = strconv.Itoa(int(offset) + limit)
	}

	return output, nil
}
