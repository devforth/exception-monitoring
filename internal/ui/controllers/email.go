package controllers

import (
	"net/http"
	"strconv"

	"github.com/devforth/exception-monitoring/internal/services"
)

// The email step: htmx swaps the "email-step" fragment as the address goes
// from entered to confirmed (codes: services.Verification).

func (controller *Controllers) renderEmailStep(w http.ResponseWriter, r *http.Request, step services.EmailStep) {
	// The step of an existing connection keeps its address: no "Change email".
	step.Fixed = r.PostFormValue("address_fixed") != ""

	controller.views.RenderPartial(w, r, "connection", "email-step", step)
}

// EmailStart takes the entered address: a confirmed one is accepted at once,
// otherwise a code is mailed.
func (controller *Controllers) EmailStart(w http.ResponseWriter, r *http.Request) {
	step, err := controller.verification.StartEmail(r.Context(), r.PostFormValue("address"))

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	controller.renderEmailStep(w, r, step)
}

func (controller *Controllers) EmailResend(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PostFormValue("destination_id"), 10, 64)
	step, err := controller.verification.ResendCode(r.Context(), id)

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	controller.renderEmailStep(w, r, step)
}

func (controller *Controllers) EmailConfirm(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PostFormValue("destination_id"), 10, 64)
	step, err := controller.verification.ConfirmCode(r.Context(), id, r.PostFormValue("code"))

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	controller.renderEmailStep(w, r, step)
}

// EmailChange goes back to entering an address.
func (controller *Controllers) EmailChange(w http.ResponseWriter, r *http.Request) {
	controller.renderEmailStep(w, r, services.EmailStep{State: "input", Address: r.PostFormValue("address")})
}
