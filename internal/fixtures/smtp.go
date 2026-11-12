package fixtures

import (
	"bufio"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"strings"
	"testing"

	"github.com/devforth/exception-monitoring/internal/config"
)

// SMTPSession is what the fake SMTP server received in one session.
type SMTPSession struct {
	Commands []string
	Data     string // the message as sent after DATA
}

// FakeSMTP is a minimal SMTP server on localhost without TLS, accepting any
// number of sessions. net/smtp allows AUTH PLAIN over plain text only to
// localhost, which is exactly this case. Each finished session is sent on
// the returned channel.
func FakeSMTP(t testing.TB) (config.SMTP, <-chan SMTPSession) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = listener.Close() })

	sessions := make(chan SMTPSession, 16)

	go func() {
		for {
			conn, err := listener.Accept()

			if err != nil {
				return
			}

			go serveSMTP(conn, sessions)
		}
	}()

	return config.SMTP{
		Host:     "127.0.0.1",
		Port:     listener.Addr().(*net.TCPAddr).Port,
		Username: "postmaster",
		Password: "secret",
		From:     "Errors <errors@example.com>",
	}, sessions
}

func serveSMTP(conn net.Conn, sessions chan<- SMTPSession) {
	defer func() { _ = conn.Close() }()

	reader := bufio.NewReader(conn)
	reply := func(line string) { _, _ = io.WriteString(conn, line+"\r\n") }

	var session SMTPSession

	reply("220 localhost ESMTP")

	for {
		line, err := reader.ReadString('\n')

		if err != nil {
			return
		}

		command := strings.TrimRight(line, "\r\n")
		session.Commands = append(session.Commands, command)

		switch upper := strings.ToUpper(command); {
		case strings.HasPrefix(upper, "EHLO"):
			reply("250-localhost")
			reply("250 AUTH PLAIN")
		case strings.HasPrefix(upper, "AUTH"):
			reply("235 authenticated")
		case upper == "DATA":
			reply("354 go ahead")

			var data strings.Builder

			for {
				dataLine, err := reader.ReadString('\n')

				if err != nil || dataLine == ".\r\n" {
					break
				}

				data.WriteString(dataLine)
			}

			session.Data = data.String()

			reply("250 queued")
		case upper == "QUIT":
			reply("221 bye")

			sessions <- session

			return
		default: // MAIL, RCPT, RSET, NOOP
			reply("250 ok")
		}
	}
}

// ParseEmail returns the decoded subject and the text and HTML parts of a
// message (multipart/alternative, possibly inside multipart/mixed).
func ParseEmail(t testing.TB, raw string) (subject, text, html string) {
	t.Helper()

	email := ReadEmail(t, raw)

	return email.Subject, email.Text, email.HTML
}

// Email is a parsed message: its subject, body parts and attached files.
type Email struct {
	Subject     string
	Text        string
	HTML        string
	Attachments map[string]string // file name → decoded content
}

// ReadEmail parses a message the way a mail client does.
func ReadEmail(t testing.TB, raw string) Email {
	t.Helper()

	message, err := mail.ReadMessage(strings.NewReader(raw))

	if err != nil {
		t.Fatal(err)
	}

	email := Email{Attachments: map[string]string{}}

	email.Subject, err = new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject"))

	if err != nil {
		t.Fatal(err)
	}

	readPart(t, &email, message.Header.Get("Content-Type"), "", "", message.Body)

	return email
}

func readPart(t testing.TB, email *Email, contentType, disposition, encoding string, body io.Reader) {
	t.Helper()

	mediaType, params, err := mime.ParseMediaType(contentType)

	if err != nil {
		t.Fatal(err)
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		reader := multipart.NewReader(body, params["boundary"])

		for {
			part, err := reader.NextPart()

			if err == io.EOF {
				return
			}

			if err != nil {
				t.Fatal(err)
			}

			// multipart decodes quoted-printable itself, base64 is left to us.
			readPart(t, email, part.Header.Get("Content-Type"), part.Header.Get("Content-Disposition"), part.Header.Get("Content-Transfer-Encoding"), part)
		}
	}

	if strings.EqualFold(encoding, "base64") {
		body = base64.NewDecoder(base64.StdEncoding, body)
	}

	content, err := io.ReadAll(body)

	if err != nil {
		t.Fatal(err)
	}

	if _, dispositionParams, err := mime.ParseMediaType(disposition); err == nil && dispositionParams["filename"] != "" {
		email.Attachments[dispositionParams["filename"]] = string(content)

		return
	}

	if mediaType == "text/html" {
		email.HTML = string(content)
	} else {
		email.Text = string(content)
	}
}
