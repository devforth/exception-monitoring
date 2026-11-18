package services

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/storetest"

	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/platform/secret"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

func openStore(t *testing.T) store.Store {
	t.Helper()

	database := storetest.Open(t)

	if err := database.SeedProject(context.Background(), store.Project{ID: 1, Name: "backend", PublicKey: "0123456789abcdef0123456789abcdef"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	return database
}

func testFactory(t *testing.T) notify.Factory {
	t.Helper()

	box, err := secret.New("test-secret-key-test-secret-key-0123")

	if err != nil {
		t.Fatal(err)
	}

	return notify.Factory{Box: box}
}

func TestConnectionsCreateUpdateDelete(t *testing.T) {
	ctx := context.Background()
	database := openStore(t)
	factory := testFactory(t)
	service := NewConnections(slog.New(slog.NewTextHandler(io.Discard, nil)), database, factory, config.Projects{}, &config.Config{})
	firstSecret, _ := factory.EncryptSecret(notify.SlackSecret{WebhookURL: "https://hooks.slack.com/services/first"})

	connection := store.Connection{Name: "alerts", Type: config.ChannelSlack, AllProjects: true, Levels: []string{"error"}, Active: true}

	if _, err := service.create(ctx, connection, firstSecret); err != nil {
		t.Fatal(err)
	}

	connections, err := service.List(ctx)

	if err != nil || len(connections) != 1 {
		t.Fatalf("connections = %v, %v; want the new one", connections, err)
	}

	created := connections[0]

	// A chat needs no confirmation: its destination is created confirmed, holding the secret.
	if created.DestinationID == 0 || !created.Verified || created.Secret != firstSecret {
		t.Errorf("created = %+v, want a confirmed destination with the secret", created)
	}

	secondSecret, _ := factory.EncryptSecret(notify.SlackSecret{WebhookURL: "https://hooks.slack.com/services/second"})
	created.Name = "renamed"

	if err := service.update(ctx, created, secondSecret); err != nil {
		t.Fatal(err)
	}

	updated, err := service.Get(ctx, created.ID)

	if err != nil || updated.Name != "renamed" || updated.Secret != secondSecret || updated.DestinationID != created.DestinationID {
		t.Errorf("updated = %+v, %v; want the new name and secret on the same destination", updated, err)
	}

	if err := service.SetActive(ctx, updated, false); err != nil {
		t.Fatal(err)
	}

	if inactive, _ := service.Get(ctx, created.ID); inactive.Active {
		t.Errorf("connection still active after SetActive(false)")
	}

	if err := service.Delete(ctx, created.ID); err != nil {
		t.Fatal(err)
	}

	// Deleted from another tab: not an error.
	if err := service.Delete(ctx, created.ID); err != nil {
		t.Errorf("second delete = %v, want nil", err)
	}

	if _, err := service.Get(ctx, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get after delete = %v, want ErrNotFound", err)
	}
}

func TestVerificationDestinations(t *testing.T) {
	ctx := context.Background()
	database := openStore(t)
	verification := NewVerification(slog.New(slog.NewTextHandler(io.Discard, nil)), database, notify.Factory{}, nil)

	first, err := verification.DestinationFor(ctx, "dev@example.com")

	if err != nil || first.ID == 0 || first.Verified || first.Type != config.ChannelEmail {
		t.Fatalf("DestinationFor = %+v, %v; want a new unconfirmed email destination", first, err)
	}

	again, err := verification.DestinationFor(ctx, "dev@example.com")

	if err != nil || again.ID != first.ID {
		t.Errorf("second DestinationFor = %d, %v; want the same destination %d", again.ID, err, first.ID)
	}

	if found, err := verification.EmailDestination(ctx, first.ID); err != nil || found.Address != "dev@example.com" {
		t.Errorf("EmailDestination = %+v, %v", found, err)
	}

	chatID, err := database.CreateDestination(ctx, store.Destination{Type: config.ChannelSlack, Secret: "x", Verified: true}, time.Now())

	if err != nil {
		t.Fatal(err)
	}

	if _, err := verification.EmailDestination(ctx, chatID); !errors.Is(err, ErrNotEmail) {
		t.Errorf("EmailDestination of a chat = %v, want ErrNotEmail", err)
	}
}

func TestSelectedEvent(t *testing.T) {
	ctx := context.Background()
	database := openStore(t)
	issues := NewIssues(database)
	now := time.Now().UTC()

	save := func(eventID, fingerprint string, received time.Time) int64 {
		result, err := database.SaveEvent(ctx, store.NewEvent{
			ProjectID: 1, EventID: eventID, Fingerprint: fingerprint, Title: fingerprint, Level: "error",
			Timestamp: received, ReceivedAt: received, Payload: []byte(`{}`),
		})

		if err != nil {
			t.Fatal(err)
		}

		return result.IssueID
	}

	issueID := save("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "first", now.Add(-time.Minute))
	save("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "first", now)
	save("cccccccccccccccccccccccccccccccc", "other", now)

	issue, err := issues.Get(ctx, issueID)

	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name, eventID, want string
		err                 error
	}{
		{"latest by default", "", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", nil},
		{"UUID form", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil},
		{"event of another issue", "cccccccccccccccccccccccccccccccc", "", store.ErrNotFound},
		{"unknown event", "dddddddddddddddddddddddddddddddd", "", store.ErrNotFound},
	} {
		event, err := issues.SelectedEvent(ctx, issue, test.eventID)

		if !errors.Is(err, test.err) || event.EventID != test.want {
			t.Errorf("%s: event %q, err %v; want %q, %v", test.name, event.EventID, err, test.want, test.err)
		}
	}
}

// Saving an active connection retries its waiting messages now: a save is
// usually the fix for why they failed.
func TestSaveRetriesWaitingMessages(t *testing.T) {
	database := storetest.Open(t)

	ctx := context.Background()
	destination, _ := database.CreateDestination(ctx, store.Destination{Type: "slack", Secret: "x", Verified: true}, time.Now())
	id, _ := database.CreateConnection(ctx, store.Connection{Name: "Chat", DestinationID: destination, AllProjects: true, Levels: []string{"error"}, Active: true}, time.Now())
	channel := strconv.FormatInt(id, 10)

	if err := database.EnqueueOutbox(ctx, []string{channel}, "notification", []byte(`{}`), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	messages, _ := database.DueOutbox(ctx, channel, time.Now(), 10)
	_ = database.MarkOutboxRetry(ctx, messages[0].ID, "slack: missing_scope", time.Now().Add(5*time.Minute), time.Now())

	if due, _ := database.DueOutbox(ctx, channel, time.Now(), 10); len(due) != 0 {
		t.Fatal("the message must wait for its retry")
	}

	connection, _ := database.GetConnection(ctx, id)
	connections := NewConnections(slog.New(slog.DiscardHandler), database, notify.Factory{}, config.Projects{}, &config.Config{})

	if err := connections.update(ctx, connection, ""); err != nil {
		t.Fatal(err)
	}

	if due, _ := database.DueOutbox(ctx, channel, time.Now().Add(time.Second), 10); len(due) != 1 {
		t.Fatal("saving the connection must make its waiting message due now")
	}
}
