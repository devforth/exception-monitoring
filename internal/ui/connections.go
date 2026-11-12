package ui

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/mail"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/store"
)

// Notification connections: where alerts go (an email address, a Telegram
// chat, a Slack webhook) and which projects and levels each one receives.
//
// An email address is confirmed before the rest of the form can be filled:
// "Verify" mails a code, entering it confirms the address. The confirmation
// belongs to the address (a destination), not to the connection, so adding
// a confirmed address again — after deleting a connection or leaving the
// form — needs no new code.

const verificationCodeLifetime = 15 * time.Minute

// resendCooldown is the minimum time between two codes to one address, so
// the button cannot be used to flood a mailbox. A variable for tests.
var resendCooldown = 30 * time.Second

var (
	telegramTokenRe = regexp.MustCompile(`^\d+:[A-Za-z0-9_-]{20,}$`)
	telegramChatRe  = regexp.MustCompile(`^(-?\d+|@[A-Za-z0-9_]{5,})$`)
)

var channelLabels = map[string]string{
	config.ChannelEmail:    "Email",
	config.ChannelTelegram: "Telegram",
	config.ChannelSlack:    "Slack",
}

type typeChoice struct {
	Value, Label, Reason string
	Available, Selected  bool
}

type choice struct {
	Value   string
	Label   string
	Checked bool
}

// emailStep is the address part of the form, swapped by htmx as it moves on:
// State "input" (enter an address), "unconfirmed" (an existing connection
// whose address is not confirmed yet: send a code), "code" (enter the mailed
// code) or "verified" (confirmed: the rest of the form opens). Fixed is set
// for an existing connection, whose address cannot change.
type emailStep struct {
	State         string
	Address       string
	DestinationID int64
	VerifiedAt    time.Time
	Fixed         bool
	Duplicate     *duplicate // the connection that already uses the address
	Error         string
	Notice        string
}

// duplicate names the connection that already sends to an address or chat,
// so the error can link to it.
type duplicate struct {
	ID   int64
	Name string
}

// connectionView is what templates/connection.html shows: the form of a new
// or an existing connection, with errors after a failed save.
type connectionView struct {
	ID            int64
	New           bool
	Name          string
	Type          string
	TypeLabel     string
	Email         emailStep
	ChatID        string // shown; the bot token never is
	HasSecret     bool   // a token/webhook is stored and kept when the field is left empty
	AllProjects   bool
	Active        bool
	HideIssueLink bool
	Verified      bool
	Types         []typeChoice
	Projects      []choice
	Levels        []choice
	Errors        map[string]string
	Duplicate     *duplicate // a chat already used by another connection
	Error         string     // a failure that is not about one field (sending, saving)
	Notice        string
}

type connectionRow struct {
	ID          int64
	Name        string
	TypeLabel   string
	Destination string
	Projects    string
	Levels      string
	Status      string
	StatusClass string // active, inactive or awaiting
	Active      bool
	Verified    bool
}

type connectionsView struct {
	Rows      []connectionRow
	CanCreate bool
	Notice    string
}

var notices = map[string]string{
	"saved":       "Saved.",
	"created":     "Connection added.",
	"test-sent":   "Test message sent.",
	"deleted":     "Connection deleted.",
	"activated":   "Connection activated.",
	"deactivated": "Connection deactivated.",
}

func (handler *Handler) connectionsPage(w http.ResponseWriter, r *http.Request) {
	connections, err := handler.store.ListConnections(r.Context())

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	view := connectionsView{Notice: notices[r.URL.Query().Get("notice")]}

	for _, connection := range connections {
		view.Rows = append(view.Rows, handler.connectionRow(connection))
	}

	for _, channelType := range config.AllChannelTypes {
		if available, _ := handler.notifications.ChannelAvailable(channelType); available {
			view.CanCreate = true
		}
	}

	handler.render(w, r, http.StatusOK, "notifications", view)
}

func (handler *Handler) connectionRow(connection store.Connection) connectionRow {
	row := connectionRow{ID: connection.ID, Name: connection.Name, TypeLabel: channelLabels[connection.Type], Levels: strings.Join(connection.Levels, ", ")}
	row.Active, row.Verified = connection.Active, connection.Verified

	switch connection.Type {
	case config.ChannelEmail:
		row.Destination = connection.Address
	case config.ChannelTelegram:
		var tokens notify.TelegramSecret

		if handler.factory.DecryptSecret(connection.Secret, &tokens) == nil {
			row.Destination = "chat " + tokens.ChatID
		} else {
			row.Destination = "token cannot be decrypted (SECRET_KEY changed?)"
		}
	case config.ChannelSlack:
		row.Destination = "incoming webhook"
	}

	if connection.AllProjects {
		row.Projects = "All projects"
	} else {
		var names []string

		for _, projectID := range connection.ProjectIDs {
			names = append(names, handler.projectName(projectID))
		}

		row.Projects = strings.Join(names, ", ")
	}

	switch {
	case !connection.Verified:
		row.Status, row.StatusClass = "awaiting confirmation", "awaiting"
	case connection.Active:
		row.Status, row.StatusClass = "active", "active"
	default:
		row.Status, row.StatusClass = "inactive", "inactive"
	}

	return row
}

// newConnectionView fills the form from a connection (empty for a new one).
func (handler *Handler) newConnectionView(connection store.Connection) connectionView {
	view := connectionView{
		ID: connection.ID, New: connection.ID == 0, Name: connection.Name, Type: connection.Type,
		TypeLabel: channelLabels[connection.Type], AllProjects: connection.AllProjects,
		HideIssueLink: connection.HideIssueLink,
		Active:        connection.Active, Verified: connection.Verified, HasSecret: connection.Secret != "", Errors: map[string]string{},
		Email: emailStep{State: "input", Address: connection.Address, DestinationID: connection.DestinationID},
	}

	if connection.Type == config.ChannelEmail && !view.New {
		view.Email.Fixed = true
		view.Email.State = "unconfirmed"
	}

	if connection.Type == config.ChannelEmail && connection.Verified {
		view.Email.State = "verified"
	}

	for _, channelType := range config.AllChannelTypes {
		available, reason := handler.notifications.ChannelAvailable(channelType)
		view.Types = append(view.Types, typeChoice{
			Value: channelType, Label: channelLabels[channelType], Reason: reason,
			Available: available, Selected: channelType == connection.Type,
		})
	}

	for _, project := range handler.projects {
		view.Projects = append(view.Projects, choice{
			Value: strconv.Itoa(project.ID), Label: project.Name, Checked: slices.Contains(connection.ProjectIDs, project.ID),
		})
	}

	for _, level := range config.Levels {
		view.Levels = append(view.Levels, choice{Value: level, Label: level, Checked: slices.Contains(connection.Levels, level)})
	}

	if connection.Type == config.ChannelTelegram && connection.Secret != "" {
		var tokens notify.TelegramSecret

		if handler.factory.DecryptSecret(connection.Secret, &tokens) == nil {
			view.ChatID = tokens.ChatID
		}
	}

	return view
}

// RestLocked tells the template to disable everything after the address
// until the address of an email connection is confirmed.
func (view connectionView) RestLocked() bool {
	return view.Type == config.ChannelEmail && view.Email.State != "verified"
}

func (handler *Handler) newConnectionPage(w http.ResponseWriter, r *http.Request) {
	connection := store.Connection{AllProjects: true, Levels: []string{"error", "fatal"}, Active: true}

	// Preselect the first type that can be used.
	for _, channelType := range config.AllChannelTypes {
		if available, _ := handler.notifications.ChannelAvailable(channelType); available {
			connection.Type = channelType

			break
		}
	}

	handler.render(w, r, http.StatusOK, "connection", handler.newConnectionView(connection))
}

// loadConnection reads the connection of the {connectionID} path value; on
// failure it has written the response (404 or 500).
func (handler *Handler) loadConnection(w http.ResponseWriter, r *http.Request) (store.Connection, bool) {
	id, err := strconv.ParseInt(r.PathValue("connectionID"), 10, 64)

	if err != nil {
		http.NotFound(w, r)

		return store.Connection{}, false
	}

	connection, err := handler.store.GetConnection(r.Context(), id)

	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)

		return connection, false
	case err != nil:
		handler.serverError(w, r, err)

		return connection, false
	}

	return connection, true
}

func (handler *Handler) connectionPage(w http.ResponseWriter, r *http.Request) {
	connection, ok := handler.loadConnection(w, r)

	if !ok {
		return
	}

	view := handler.newConnectionView(connection)
	view.Notice = notices[r.URL.Query().Get("notice")]

	if connection.Type == config.ChannelEmail {
		if destination, err := handler.store.GetDestination(r.Context(), connection.DestinationID); err == nil {
			view.Email.VerifiedAt = destination.VerifiedAt
		}
	}

	handler.render(w, r, http.StatusOK, "connection", view)
}

// readConnectionForm applies the rule fields (name, projects, levels,
// active) to connection and returns the field errors.
func (handler *Handler) readConnectionForm(r *http.Request, connection *store.Connection) map[string]string {
	problems := map[string]string{}

	connection.Name = strings.TrimSpace(r.PostFormValue("name"))

	if connection.Name == "" || len([]rune(connection.Name)) > 100 {
		problems["name"] = "Enter a name of up to 100 characters."
	}

	connection.AllProjects = r.PostFormValue("all_projects") == "on"
	connection.ProjectIDs = nil

	if !connection.AllProjects {
		for _, value := range r.PostForm["project"] {
			projectID, err := strconv.Atoi(value)

			if _, ok := handler.projects.ByID(projectID); err == nil && ok {
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

	if connection.Type == config.ChannelEmail {
		connection.HideIssueLink = r.PostFormValue("include_issue_link") != "on"
	}

	return problems
}

// readChatSecret reads the tokens of a chat. existing is the stored secret:
// an empty field keeps its value. Returns the encrypted secret and the
// chat's identity (see destinationKey).
func (handler *Handler) readChatSecret(r *http.Request, channelType, existing string, problems map[string]string) (string, string) {
	var (
		tokens any
		key    string
	)

	switch channelType {
	case config.ChannelTelegram:
		var telegram notify.TelegramSecret

		if existing != "" {
			_ = handler.factory.DecryptSecret(existing, &telegram)
		}

		if value := strings.TrimSpace(r.PostFormValue("bot_token")); value != "" {
			telegram.BotToken = value
		}

		if value := strings.TrimSpace(r.PostFormValue("chat_id")); value != "" {
			telegram.ChatID = value
		}

		if !telegramTokenRe.MatchString(telegram.BotToken) {
			problems["bot_token"] = "Enter the bot token from @BotFather (like 123456:ABC-...)."
		}

		if !telegramChatRe.MatchString(telegram.ChatID) {
			problems["chat_id"] = "Enter the chat ID (a number, negative for groups) or @channelname."
		}

		tokens, key = telegram, telegram.ChatID
	case config.ChannelSlack:
		var slack notify.SlackSecret

		if existing != "" {
			_ = handler.factory.DecryptSecret(existing, &slack)
		}

		if value := strings.TrimSpace(r.PostFormValue("webhook_url")); value != "" {
			slack.WebhookURL = value
		}

		if err := config.CheckHTTPURL(slack.WebhookURL); err != nil {
			problems["webhook_url"] = "Enter the incoming webhook URL (https://hooks.slack.com/services/...)."
		}

		tokens, key = slack, slack.WebhookURL
	default:
		return "", ""
	}

	if len(problems) > 0 {
		return "", ""
	}

	encrypted, err := handler.factory.EncryptSecret(tokens)

	if err != nil {
		problems["type"] = "Cannot store the token: " + err.Error()
	}

	return encrypted, key
}

// destinationKey is what makes two connections send to the same place: the
// email address, the Telegram chat or the Slack webhook. Chat secrets are
// encrypted with a random nonce, so they are compared decrypted.
func (handler *Handler) destinationKey(connection store.Connection) string {
	switch connection.Type {
	case config.ChannelEmail:
		return strings.ToLower(connection.Address)
	case config.ChannelTelegram:
		var telegram notify.TelegramSecret

		if handler.factory.DecryptSecret(connection.Secret, &telegram) == nil {
			return telegram.ChatID
		}
	case config.ChannelSlack:
		var slack notify.SlackSecret

		if handler.factory.DecryptSecret(connection.Secret, &slack) == nil {
			return slack.WebhookURL
		}
	}

	return ""
}

// findDuplicate returns the connection other than exceptID that already
// sends to key, or nil.
func (handler *Handler) findDuplicate(ctx context.Context, channelType, key string, exceptID int64) (*duplicate, error) {
	if key == "" {
		return nil, nil
	}

	connections, err := handler.store.ListConnections(ctx)

	if err != nil {
		return nil, err
	}

	for _, connection := range connections {
		if connection.ID != exceptID && connection.Type == channelType && handler.destinationKey(connection) == key {
			return &duplicate{ID: connection.ID, Name: connection.Name}, nil
		}
	}

	return nil, nil
}

func (handler *Handler) createConnection(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()

	connection := store.Connection{Type: r.PostFormValue("type")}
	problems := handler.readConnectionForm(r, &connection)

	if available, reason := handler.notifications.ChannelAvailable(connection.Type); !available {
		problems["type"] = "This type is not available: " + reason + "."
	}

	var chatSecret, key string

	switch connection.Type {
	case config.ChannelEmail:
		// The address must have been confirmed in the email step of this form.
		destinationID, _ := strconv.ParseInt(r.PostFormValue("destination_id"), 10, 64)
		destination, err := handler.store.GetDestination(r.Context(), destinationID)

		if err != nil || destination.Type != config.ChannelEmail || !destination.Verified {
			problems["address"] = "Confirm the email address first."
		} else {
			connection.DestinationID, connection.Address, connection.Verified = destination.ID, destination.Address, true
			key = destination.Address
		}
	case config.ChannelTelegram, config.ChannelSlack:
		chatSecret, key = handler.readChatSecret(r, connection.Type, "", problems)
	}

	existing, err := handler.findDuplicate(r.Context(), connection.Type, key, 0)

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	if existing != nil {
		problems["duplicate"] = "already added"
	}

	if len(problems) > 0 {
		handler.renderConnectionForm(w, r, connection, problems, "", existing)

		return
	}

	if chatSecret != "" {
		// Chats need no confirmation: whoever has the token owns the chat.
		destinationID, err := handler.store.CreateDestination(r.Context(), store.Destination{Type: connection.Type, Secret: chatSecret, Verified: true, VerifiedAt: handler.now()}, handler.now())

		if err != nil {
			handler.serverError(w, r, err)

			return
		}

		connection.DestinationID = destinationID
	}

	if _, err := handler.store.CreateConnection(r.Context(), connection, handler.now()); err != nil {
		handler.serverError(w, r, err)

		return
	}

	redirectAfterPost(w, r, "/notifications?notice=created")
}

func (handler *Handler) updateConnection(w http.ResponseWriter, r *http.Request) {
	connection, ok := handler.loadConnection(w, r)

	if !ok {
		return
	}

	_ = r.ParseForm()

	problems := handler.readConnectionForm(r, &connection)
	chatSecret, key := handler.readChatSecret(r, connection.Type, connection.Secret, problems)

	if connection.Active && !connection.Verified {
		problems["active"] = "Confirm the email address first."
	}

	// A replaced token must not point to a chat another connection uses.
	existing, err := handler.findDuplicate(r.Context(), connection.Type, key, connection.ID)

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	if existing != nil {
		problems["duplicate"] = "already added"
	}

	if len(problems) > 0 {
		handler.renderConnectionForm(w, r, connection, problems, "", existing)

		return
	}

	if chatSecret != "" && chatSecret != connection.Secret {
		if err := handler.store.UpdateDestinationSecret(r.Context(), connection.DestinationID, chatSecret, handler.now()); err != nil {
			handler.serverError(w, r, err)

			return
		}
	}

	if err := handler.store.UpdateConnection(r.Context(), connection, handler.now()); err != nil {
		handler.serverError(w, r, err)

		return
	}

	redirectAfterPost(w, r, fmt.Sprintf("/notifications/%d?notice=saved", connection.ID))
}

func (handler *Handler) renderConnectionForm(w http.ResponseWriter, r *http.Request, connection store.Connection, problems map[string]string, failure string, existing *duplicate) {
	view := handler.newConnectionView(connection)
	view.Error = failure
	view.Duplicate = existing

	if problems != nil {
		view.Errors = problems
	}

	// Keep the email step where it was: a confirmed address stays confirmed.
	if connection.ID == 0 && connection.Type == config.ChannelEmail {
		view.Email.Address = r.PostFormValue("address")

		if connection.DestinationID != 0 {
			view.Email.State = "verified"
		}

		// The address is taken: back to the address field, with a link.
		if existing != nil {
			view.Email = emailStep{State: "input", Address: connection.Address, Duplicate: existing}
			view.Duplicate = nil
		}
	}

	status := http.StatusOK

	if len(problems) > 0 {
		status = http.StatusUnprocessableEntity
	}

	handler.render(w, r, status, "connection", view)
}

// The email step: htmx swaps the "email-step" fragment as the address goes
// from entered to confirmed.

func (handler *Handler) renderEmailStep(w http.ResponseWriter, r *http.Request, step emailStep) {
	// The step of an existing connection keeps its address: no "Change email".
	step.Fixed = r.PostFormValue("address_fixed") != ""

	handler.renderPartial(w, r, "connection", "email-step", step)
}

// emailStart takes the entered address: a confirmed one is accepted at once,
// otherwise a code is mailed.
func (handler *Handler) emailStart(w http.ResponseWriter, r *http.Request) {
	parsed, err := mail.ParseAddress(strings.TrimSpace(r.PostFormValue("address")))

	if err != nil {
		handler.renderEmailStep(w, r, emailStep{State: "input", Address: r.PostFormValue("address"), Error: "Enter a valid email address."})

		return
	}

	address := strings.ToLower(parsed.Address)

	// One connection per address: edit the existing one instead.
	existing, err := handler.findDuplicate(r.Context(), config.ChannelEmail, address, 0)

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	if existing != nil {
		handler.renderEmailStep(w, r, emailStep{State: "input", Address: address, Duplicate: existing})

		return
	}

	destination, err := handler.store.FindEmailDestination(r.Context(), address)

	if errors.Is(err, store.ErrNotFound) {
		id, createErr := handler.store.CreateDestination(r.Context(), store.Destination{Type: config.ChannelEmail, Address: address}, handler.now())

		if createErr != nil {
			handler.serverError(w, r, createErr)

			return
		}

		destination, err = handler.store.GetDestination(r.Context(), id)
	}

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	if destination.Verified {
		handler.renderEmailStep(w, r, emailStep{State: "verified", Address: address, DestinationID: destination.ID, VerifiedAt: destination.VerifiedAt,
			Notice: "Confirmed earlier: no code needed."})

		return
	}

	handler.sendCode(w, r, destination)
}

// sendCode mails a new code unless one was sent within resendCooldown.
func (handler *Handler) sendCode(w http.ResponseWriter, r *http.Request, destination store.Destination) {
	step := emailStep{State: "code", Address: destination.Address, DestinationID: destination.ID}

	if wait := destination.VerifySentAt.Add(resendCooldown).Sub(handler.now()); wait > 0 {
		step.Notice = fmt.Sprintf("A code was sent less than %s ago; check the inbox or send again in %ds.", resendCooldown, int(wait.Seconds())+1)
		handler.renderEmailStep(w, r, step)

		return
	}

	code := newVerificationCode()
	now := handler.now()

	if err := handler.store.SetVerificationCode(r.Context(), destination.ID, codeHash(destination.ID, code), now.Add(verificationCodeLifetime), now); err != nil {
		handler.serverError(w, r, err)

		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)

	defer cancel()

	email := notify.Email{SMTP: handler.factory.SMTP, To: destination.Address}

	if err := email.SendVerificationCode(ctx, destination.Address, code, verificationCodeLifetime); err != nil {
		handler.logger.Error("send verification code", "destination_id", destination.ID, "err", err)

		step.Error = "The email could not be sent: " + err.Error()
	} else {
		step.Notice = "We sent a 6-digit code to " + destination.Address + ". It is valid for 15 minutes."
	}

	handler.renderEmailStep(w, r, step)
}

// emailDestination reads the unconfirmed email destination named by the form.
func (handler *Handler) emailDestination(w http.ResponseWriter, r *http.Request) (store.Destination, bool) {
	id, _ := strconv.ParseInt(r.PostFormValue("destination_id"), 10, 64)
	destination, err := handler.store.GetDestination(r.Context(), id)

	if err != nil || destination.Type != config.ChannelEmail {
		handler.renderEmailStep(w, r, emailStep{State: "input", Error: "Enter the email address again."})

		return destination, false
	}

	return destination, true
}

func (handler *Handler) emailResend(w http.ResponseWriter, r *http.Request) {
	destination, ok := handler.emailDestination(w, r)

	if !ok {
		return
	}

	if destination.Verified {
		handler.renderEmailStep(w, r, emailStep{State: "verified", Address: destination.Address, DestinationID: destination.ID, VerifiedAt: destination.VerifiedAt})

		return
	}

	handler.sendCode(w, r, destination)
}

func (handler *Handler) emailConfirm(w http.ResponseWriter, r *http.Request) {
	destination, ok := handler.emailDestination(w, r)

	if !ok {
		return
	}

	step := emailStep{State: "code", Address: destination.Address, DestinationID: destination.ID}
	code := strings.TrimSpace(r.PostFormValue("code"))

	result, err := handler.store.CheckVerificationCode(r.Context(), destination.ID, codeHash(destination.ID, code), handler.now())

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	switch result {
	case store.VerifyOK:
		step.State, step.VerifiedAt, step.Notice = "verified", handler.now(), "Address confirmed."
	case store.VerifyExpired:
		step.Error = "The code has expired. Send a new one."
	case store.VerifyTooManyTries:
		step.Error = "Too many wrong codes. Send a new one."
	default:
		step.Error = "Wrong code."
	}

	handler.renderEmailStep(w, r, step)
}

// emailChange goes back to entering an address.
func (handler *Handler) emailChange(w http.ResponseWriter, r *http.Request) {
	handler.renderEmailStep(w, r, emailStep{State: "input", Address: r.PostFormValue("address")})
}

// codeHash is what is stored instead of the code: tied to the destination,
// so a code cannot confirm another address.
func codeHash(destinationID int64, code string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%d:%s", destinationID, code))

	return hex.EncodeToString(sum[:])
}

func newVerificationCode() string {
	number, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))

	return fmt.Sprintf("%06d", number.Int64())
}

func (handler *Handler) testConnection(w http.ResponseWriter, r *http.Request) {
	connection, ok := handler.loadConnection(w, r)

	if !ok {
		return
	}

	// An unconfirmed address must not receive mail beyond the code.
	if !connection.Verified {
		handler.renderConnectionForm(w, r, connection, nil, "This address is not confirmed: no test message is sent.", nil)

		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)

	defer cancel()

	if err := handler.factory.SendTest(ctx, connection, handler.notifications.PublicURL); err != nil {
		handler.renderConnectionForm(w, r, connection, nil, "The test message was not delivered: "+err.Error(), nil)

		return
	}

	redirectAfterPost(w, r, fmt.Sprintf("/notifications/%d?notice=test-sent", connection.ID))
}

func (handler *Handler) setConnectionActive(w http.ResponseWriter, r *http.Request) {
	connection, ok := handler.loadConnection(w, r)

	if !ok {
		return
	}

	active := r.PostFormValue("active")

	if active != "0" && active != "1" {
		http.Error(w, "Invalid active value", http.StatusBadRequest)

		return
	}

	connection.Active = active == "1"

	if connection.Active && !connection.Verified {
		handler.renderConnectionForm(w, r, connection, map[string]string{"active": "Confirm the email address first."}, "", nil)

		return
	}

	if err := handler.store.UpdateConnection(r.Context(), connection, handler.now()); err != nil {
		handler.serverError(w, r, err)

		return
	}

	notice := "deactivated"

	if connection.Active {
		notice = "activated"
	}

	redirectAfterPost(w, r, "/notifications?notice="+notice)
}

func (handler *Handler) deleteConnection(w http.ResponseWriter, r *http.Request) {
	connection, ok := handler.loadConnection(w, r)

	if !ok {
		return
	}

	if err := handler.store.DeleteConnection(r.Context(), connection.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		handler.serverError(w, r, err)

		return
	}

	redirectAfterPost(w, r, "/notifications?notice=deleted")
}
