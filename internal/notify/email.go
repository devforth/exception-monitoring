package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html/template"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/agentprompt"
	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/sentry"
)

//go:embed templates/email.html
var emailTemplateFiles embed.FS

var emailTemplates = template.Must(template.New("email.html").Funcs(template.FuncMap{
	"projectColor": projectColor,
}).ParseFS(emailTemplateFiles, "templates/email.html"))

// projectColor is the color of a project badge in an email; anything other
// than "#rrggbb" (an old outbox row, a missing color) falls back to grey.
func projectColor(color string) string {
	if config.ValidColor(color) {
		return color
	}

	return "#555560"
}

// Email sends the long format, with the stack trace and context, through any
// SMTP server. Port 465 uses implicit TLS; other ports upgrade with STARTTLS
// when the server offers it.
type Email struct {
	SMTP          config.SMTP
	To            string // the address of the connection
	HideIssueLink bool
}

func (email Email) Name() string { return "email" }

// Limits that keep a message readable: the throwing part of the stack and
// the steps right before the error matter most.
const (
	maxEmailFrames      = 15
	maxEmailBreadcrumbs = 15
)

type fact struct{ Name, Value string }

type breadcrumbLine struct{ Time, Category, Message string }

// emailView is what templates/email.html receives.
type emailView struct {
	Kind         string
	Notification Notification
	Event        *sentry.Event
	Facts        []fact
	Breadcrumbs  []breadcrumbLine
	Heading      string
	Summary      Summary
	Note         string
	// PromptFile is the name of the attached agent prompt, empty without one.
	PromptFile string

	// Verification email.
	ConnectionName string
	Code           string
	ValidFor       string
}

// InAppFrames returns the frames to show for an exception: the newest
// in-app ones, or the newest frames at all when none is in-app.
func (emailView) InAppFrames(stacktrace *sentry.Stacktrace) []sentry.Frame {
	frames := stacktrace.NewestFirst()

	var inApp []sentry.Frame

	for _, frame := range frames {
		if frame.IsInApp() {
			inApp = append(inApp, frame)
		}
	}

	if len(inApp) > 0 {
		frames = inApp
	}

	return frames[:min(len(frames), maxEmailFrames)]
}

func (email Email) Send(ctx context.Context, notification Notification) error {
	if email.HideIssueLink {
		notification.IssueURL = ""
	}

	view := emailView{Kind: notification.Kind.label(), Notification: notification, Event: notification.Event}

	for _, item := range []fact{
		{"Project", notification.Project},
		{"Level", notification.Level},
		{"Environment", notification.Environment},
		{"Release", notification.Release},
		{"Time", notification.Time.UTC().Format("2006-01-02 15:04:05 UTC")},
	} {
		if item.Value != "" {
			view.Facts = append(view.Facts, item)
		}
	}

	if event := notification.Event; event != nil {
		if event.User != nil && (event.User.ID != "" || event.User.Email != "") {
			view.Facts = append(view.Facts, fact{"User", strings.TrimSpace(string(event.User.ID) + " " + event.User.Email)})
		}

		breadcrumbs := event.Breadcrumbs[max(0, len(event.Breadcrumbs)-maxEmailBreadcrumbs):]

		for _, breadcrumb := range breadcrumbs {
			line := breadcrumbLine{Category: breadcrumb.Category, Message: truncate(breadcrumb.Message, 300)}

			if !breadcrumb.Timestamp.IsZero() {
				line.Time = breadcrumb.Timestamp.UTC().Format("15:04:05")
			}

			view.Breadcrumbs = append(view.Breadcrumbs, line)
		}
	}

	// The prompt for a coding agent goes as a file: mail clients run no
	// script, so it cannot be copied with a button as on the issue page, and
	// in the body it would bury the error.
	prompt := agentprompt.Build(agentprompt.Input{
		Project: notification.Project, Title: notification.Title, Level: notification.Level, Culprit: notification.Culprit,
		Kind: notification.Kind.label(), Event: notification.Event,
	})

	subject := fmt.Sprintf("[%s] %s: %s", notification.Project, notification.Kind.label(), truncate(notification.Title, 150))
	view.PromptFile = promptFileName(notification)
	text := email.plainText(notification) + "\nA prompt for a coding agent that reproduces and explains this error is attached (" + view.PromptFile + ").\n"

	return email.send(ctx, subject, text, "notification", view, attachment{Name: view.PromptFile, ContentType: "text/markdown; charset=utf-8", Data: []byte(prompt)})
}

func (email Email) SendSummary(ctx context.Context, summary Summary) error {
	if email.HideIssueLink {
		items := append([]Notification(nil), summary.Items...)

		for index := range items {
			items[index].IssueURL = ""
		}

		summary.Items = items
	}

	view := emailView{Heading: summary.heading(), Summary: summary, Note: summary.note()}

	var text strings.Builder

	text.WriteString(summary.heading() + "\n\n")

	for _, item := range summary.Items {
		fmt.Fprintf(&text, "- [%s] %s %s\n", item.Project, item.Title, item.IssueURL)
	}

	text.WriteString("\n" + summary.note() + "\n")

	return email.send(ctx, "[Exception Monitoring] "+summary.heading(), text.String(), "summary", view)
}

// plainText is the text/plain part for clients that do not show HTML.
func (email Email) plainText(notification Notification) string {
	var text strings.Builder

	fmt.Fprintf(&text, "%s in %s (%s)\n\n%s\n", notification.Kind.label(), notification.Project, notification.Level, notification.Title)

	if details := notification.details(); details != "" {
		text.WriteString(details + "\n")
	}

	if notification.IssueURL != "" {
		text.WriteString("\n" + notification.IssueURL + "\n")
	}

	if event := notification.Event; event != nil {
		for index, exception := range event.Exception.OutermostFirst() {
			prefix := ""

			if index > 0 {
				prefix = "Caused by "
			}

			fmt.Fprintf(&text, "\n%s%s: %s\n", prefix, exception.Type, exception.Value)

			for _, frame := range (emailView{}).InAppFrames(exception.Stacktrace) {
				fmt.Fprintf(&text, "    at %s (%s:%d)\n", frame.Function, firstNonEmpty(frame.Filename, frame.Module), frame.Lineno)
			}
		}
	}

	return text.String()
}

// promptFileName names the attached prompt after the issue, such as
// "backend-error-6.md", so prompts saved from several emails do not
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

// attachment is a file attached to an email.
type attachment struct {
	Name        string
	ContentType string
	Data        []byte
}

func (email Email) send(ctx context.Context, subject, text, templateName string, view emailView, attachments ...attachment) error {
	var htmlBody bytes.Buffer

	if err := emailTemplates.ExecuteTemplate(&htmlBody, templateName, view); err != nil {
		return fmt.Errorf("email template: %w", err)
	}

	message, err := buildMessage(email.SMTP.From, []string{email.To}, subject, text, htmlBody.String(), attachments...)

	if err != nil {
		return err
	}

	return deliver(ctx, email.SMTP, []string{email.To}, message)
}

// SendVerificationCode mails the code that confirms the address of a new
// email connection. It is sent directly, not through the outbox: the person
// is waiting for it in the UI.
func (email Email) SendVerificationCode(ctx context.Context, connectionName, code string, validFor time.Duration) error {
	subject := "Confirm notifications to this address: " + code
	text := fmt.Sprintf("Your confirmation code for the notification connection %q is:\n\n    %s\n\n"+
		"Enter it in Exception Monitoring within %s. If you did not add this address, ignore this email.\n",
		connectionName, code, validFor.Round(time.Minute))

	view := emailView{ConnectionName: connectionName, Code: code, ValidFor: validFor.Round(time.Minute).String()}

	return email.send(ctx, subject, text, "verification", view)
}

// buildMessage assembles a multipart/alternative message (text and HTML),
// wrapped in multipart/mixed when files are attached.
func buildMessage(from string, to []string, subject, text, htmlBody string, attachments ...attachment) ([]byte, error) {
	sender, err := mail.ParseAddress(from)

	if err != nil {
		return nil, fmt.Errorf("sender: %w", err)
	}

	boundary := randomHex(16)
	mixedBoundary := randomHex(16)
	contentType := `multipart/alternative; boundary="` + boundary + `"`

	if len(attachments) > 0 {
		contentType = `multipart/mixed; boundary="` + mixedBoundary + `"`
	}

	var message bytes.Buffer

	headers := [][2]string{
		{"From", sender.String()},
		{"To", strings.Join(to, ", ")},
		{"Subject", mime.QEncoding.Encode("utf-8", subject)},
		{"Date", time.Now().Format(time.RFC1123Z)},
		{"Message-ID", "<" + randomHex(16) + "@" + domainOf(sender.Address) + ">"},
		{"MIME-Version", "1.0"},
		{"Content-Type", contentType},
	}

	for _, header := range headers {
		message.WriteString(header[0] + ": " + header[1] + "\r\n")
	}

	if len(attachments) > 0 {
		message.WriteString("\r\n--" + mixedBoundary + "\r\n")
		message.WriteString(`Content-Type: multipart/alternative; boundary="` + boundary + `"` + "\r\n")
	}

	for _, part := range [][2]string{{"text/plain", text}, {"text/html", htmlBody}} {
		message.WriteString("\r\n--" + boundary + "\r\n")
		message.WriteString("Content-Type: " + part[0] + "; charset=utf-8\r\n")
		message.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")

		writer := quotedprintable.NewWriter(&message)

		if _, err := writer.Write([]byte(part[1])); err != nil {
			return nil, err
		}

		if err := writer.Close(); err != nil {
			return nil, err
		}
	}

	message.WriteString("\r\n--" + boundary + "--\r\n")

	if len(attachments) == 0 {
		return message.Bytes(), nil
	}

	for _, file := range attachments {
		name := mime.QEncoding.Encode("utf-8", file.Name)

		message.WriteString("\r\n--" + mixedBoundary + "\r\n")
		message.WriteString("Content-Type: " + file.ContentType + "; name=\"" + name + "\"\r\n")
		message.WriteString("Content-Disposition: attachment; filename=\"" + name + "\"\r\n")
		message.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")

		// Base64 in lines of 76 characters, as MIME requires.
		encoded := base64.StdEncoding.EncodeToString(file.Data)

		for len(encoded) > 76 {
			message.WriteString(encoded[:76] + "\r\n")
			encoded = encoded[76:]
		}

		message.WriteString(encoded + "\r\n")
	}

	message.WriteString("\r\n--" + mixedBoundary + "--\r\n")

	return message.Bytes(), nil
}

// deliver talks SMTP: connect (TLS on 465), STARTTLS when offered, AUTH when
// a username is set, then send to every recipient.
func deliver(ctx context.Context, settings config.SMTP, recipients []string, message []byte) error {
	address := net.JoinHostPort(settings.Host, strconv.Itoa(settings.Port))
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	tlsConfig := &tls.Config{ServerName: settings.Host, MinVersion: tls.VersionTLS12}

	var (
		conn net.Conn
		err  error
	)

	if settings.Port == 465 {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}

	if err != nil {
		return fmt.Errorf("smtp connect %s: %w", address, err)
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	client, err := smtp.NewClient(conn, settings.Host)

	if err != nil {
		_ = conn.Close()

		return fmt.Errorf("smtp greeting: %w", err)
	}

	defer func() { _ = client.Close() }()

	if settings.Port != 465 {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(tlsConfig); err != nil {
				return fmt.Errorf("smtp starttls: %w", err)
			}
		}
	}

	// PlainAuth refuses to send the password over a connection that is not
	// encrypted (except to localhost), so a server without TLS fails here.
	if settings.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", settings.Username, settings.Password, settings.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	sender, _ := mail.ParseAddress(settings.From)

	if err := client.Mail(sender.Address); err != nil {
		return fmt.Errorf("smtp sender: %w", err)
	}

	for _, recipient := range recipients {
		address, err := mail.ParseAddress(recipient)

		if err != nil {
			return fmt.Errorf("smtp recipient %q: %w", recipient, err)
		}

		if err := client.Rcpt(address.Address); err != nil {
			return fmt.Errorf("smtp recipient %s: %w", address.Address, err)
		}
	}

	writer, err := client.Data()

	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}

	if _, err := writer.Write(message); err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}

	return client.Quit()
}

func randomHex(size int) string {
	value := make([]byte, size)
	_, _ = rand.Read(value)

	return hex.EncodeToString(value)
}

func domainOf(address string) string {
	if index := strings.LastIndexByte(address, '@'); index >= 0 {
		return address[index+1:]
	}

	return "localhost"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}
