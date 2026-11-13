package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
)

// An email address is confirmed before the rest of the connection form can
// be filled: "Verify" mails a code, entering it confirms the address. The
// confirmation belongs to the address (a destination), not to the
// connection, so adding a confirmed address again — after deleting a
// connection or leaving the form — needs no new code.

const verificationCodeLifetime = 15 * time.Minute

// ResendCooldown is the minimum time between two codes to one address, so
// the button cannot be used to flood a mailbox. A variable for tests.
var ResendCooldown = 30 * time.Second

// Verification mails and checks the codes that confirm email addresses.
type Verification struct {
	logger  *slog.Logger
	store   *store.Store
	factory notify.Factory
	now     func() time.Time
}

func NewVerification(logger *slog.Logger, database *store.Store, factory notify.Factory) *Verification {
	return &Verification{logger: logger, store: database, factory: factory, now: time.Now}
}

// DestinationFor returns the email destination of address (lowercase),
// creating it unconfirmed the first time the address is entered.
func (verification *Verification) DestinationFor(ctx context.Context, address string) (store.Destination, error) {
	destination, err := verification.store.FindEmailDestination(ctx, address)

	if !errors.Is(err, store.ErrNotFound) {
		return destination, err
	}

	id, err := verification.store.CreateDestination(ctx, store.Destination{Type: config.ChannelEmail, Address: address}, verification.now())

	if err != nil {
		return store.Destination{}, err
	}

	return verification.store.GetDestination(ctx, id)
}

// ErrNotEmail: the destination id a form sent is not an email destination.
var ErrNotEmail = errors.New("not an email destination")

// EmailDestination reads the email destination with id; ErrNotEmail when it
// is a chat, store.ErrNotFound when it does not exist.
func (verification *Verification) EmailDestination(ctx context.Context, id int64) (store.Destination, error) {
	destination, err := verification.store.GetDestination(ctx, id)

	if err == nil && destination.Type != config.ChannelEmail {
		return destination, ErrNotEmail
	}

	return destination, err
}

// SendCode mails a new code unless one was sent within ResendCooldown, and
// returns the step to show. An error is a failure of the database.
func (verification *Verification) SendCode(ctx context.Context, destination store.Destination) (contracts.EmailStep, error) {
	step := contracts.EmailStep{State: "code", Address: destination.Address, DestinationID: destination.ID}

	if wait := destination.VerifySentAt.Add(ResendCooldown).Sub(verification.now()); wait > 0 {
		step.Notice = fmt.Sprintf("A code was sent less than %s ago; check the inbox or send again in %ds.", ResendCooldown, int(wait.Seconds())+1)

		return step, nil
	}

	code := newVerificationCode()
	now := verification.now()

	if err := verification.store.SetVerificationCode(ctx, destination.ID, codeHash(destination.ID, code), now.Add(verificationCodeLifetime), now); err != nil {
		return step, err
	}

	sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)

	defer cancel()

	email := verification.factory.Email(destination.Address)

	if err := email.SendVerificationCode(sendCtx, destination.Address, code, verificationCodeLifetime); err != nil {
		verification.logger.Error("send verification code", "destination_id", destination.ID, "err", err)

		step.Error = "The email could not be sent: " + err.Error()
	} else {
		step.Notice = "We sent a 6-digit code to " + destination.Address + ". It is valid for 15 minutes."
	}

	return step, nil
}

// Confirm checks a typed code and returns the step to show. An error is a
// failure of the database.
func (verification *Verification) Confirm(ctx context.Context, destination store.Destination, code string) (contracts.EmailStep, error) {
	step := contracts.EmailStep{State: "code", Address: destination.Address, DestinationID: destination.ID}

	result, err := verification.store.CheckVerificationCode(ctx, destination.ID, codeHash(destination.ID, code), verification.now())

	if err != nil {
		return step, err
	}

	switch result {
	case store.VerifyOK:
		step.State, step.VerifiedAt, step.Notice = "verified", verification.now(), "Address confirmed."
	case store.VerifyExpired:
		step.Error = "The code has expired. Send a new one."
	case store.VerifyTooManyTries:
		step.Error = "Too many wrong codes. Send a new one."
	default:
		step.Error = "Wrong code."
	}

	return step, nil
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
