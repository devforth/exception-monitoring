package controllers

import (
	"net/http"
	"net/mail"
	"strconv"
	"strings"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
)

// The email step: htmx swaps the "email-step" fragment as the address goes
// from entered to confirmed (codes: services.Verification).

func (controller *Controllers) renderEmailStep(w http.ResponseWriter, r *http.Request, step contracts.EmailStep) {
	// The step of an existing connection keeps its address: no "Change email".
	step.Fixed = r.PostFormValue("address_fixed") != ""

	controller.views.RenderPartial(w, r, "connection", "email-step", step)
}

// EmailStart takes the entered address: a confirmed one is accepted at once,
// otherwise a code is mailed.
func (controller *Controllers) EmailStart(w http.ResponseWriter, r *http.Request) {
	parsed, err := mail.ParseAddress(strings.TrimSpace(r.PostFormValue("address")))

	if err != nil {
		controller.renderEmailStep(w, r, contracts.EmailStep{State: "input", Address: r.PostFormValue("address"), Error: "Enter a valid email address."})

		return
	}

	address := strings.ToLower(parsed.Address)

	// One connection per address: edit the existing one instead.
	existing, err := controller.connections.FindDuplicate(r.Context(), config.ChannelEmail, address, 0)

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	if existing != nil {
		controller.renderEmailStep(w, r, contracts.EmailStep{State: "input", Address: address, Duplicate: existing})

		return
	}

	destination, err := controller.verification.DestinationFor(r.Context(), address)

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	if destination.Verified {
		controller.renderEmailStep(w, r, contracts.EmailStep{State: "verified", Address: address, DestinationID: destination.ID, VerifiedAt: destination.VerifiedAt,
			Notice: "Confirmed earlier: no code needed."})

		return
	}

	controller.sendCode(w, r, destination)
}

// sendCode mails a new code unless one was sent within services.ResendCooldown.
func (controller *Controllers) sendCode(w http.ResponseWriter, r *http.Request, destination store.Destination) {
	step, err := controller.verification.SendCode(r.Context(), destination)

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	controller.renderEmailStep(w, r, step)
}

// emailDestination reads the unconfirmed email destination named by the form.
func (controller *Controllers) emailDestination(w http.ResponseWriter, r *http.Request) (store.Destination, bool) {
	id, _ := strconv.ParseInt(r.PostFormValue("destination_id"), 10, 64)
	destination, err := controller.verification.EmailDestination(r.Context(), id)

	if err != nil {
		controller.renderEmailStep(w, r, contracts.EmailStep{State: "input", Error: "Enter the email address again."})

		return destination, false
	}

	return destination, true
}

func (controller *Controllers) EmailResend(w http.ResponseWriter, r *http.Request) {
	destination, ok := controller.emailDestination(w, r)

	if !ok {
		return
	}

	if destination.Verified {
		controller.renderEmailStep(w, r, contracts.EmailStep{State: "verified", Address: destination.Address, DestinationID: destination.ID, VerifiedAt: destination.VerifiedAt})

		return
	}

	controller.sendCode(w, r, destination)
}

func (controller *Controllers) EmailConfirm(w http.ResponseWriter, r *http.Request) {
	destination, ok := controller.emailDestination(w, r)

	if !ok {
		return
	}

	step, err := controller.verification.Confirm(r.Context(), destination, strings.TrimSpace(r.PostFormValue("code")))

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	controller.renderEmailStep(w, r, step)
}

// EmailChange goes back to entering an address.
func (controller *Controllers) EmailChange(w http.ResponseWriter, r *http.Request) {
	controller.renderEmailStep(w, r, contracts.EmailStep{State: "input", Address: r.PostFormValue("address")})
}
