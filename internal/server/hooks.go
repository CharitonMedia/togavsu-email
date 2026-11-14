package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/CharitonMedia/togavsu-email/internal/send"
	"github.com/CharitonMedia/togavsu-email/internal/webhook"
)

func (s *Server) signed(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 40<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "bad_body"})
		return nil, false
	}
	if err := webhook.Verify(s.WebhookSecret, r.Header.Get(webhook.SignatureHeader), body, s.now(), Skew); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"code": "bad_signature", "detail": err.Error()})
		return nil, false
	}
	return body, true
}

// events records each event once; a matter.* or contact.* event re-matches the queue
// on the loop's next pass.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	body, ok := s.signed(w, r)
	if !ok {
		return
	}
	var ev webhook.Event
	if err := json.Unmarshal(body, &ev); err != nil || ev.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "bad_event"})
		return
	}
	fresh, err := s.Store.RecordEvent(r.Context(), ev.ID, ev.Action, ev.RecordID)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if fresh && s.Syncer != nil && (strings.HasPrefix(ev.Action, "matter.") || strings.HasPrefix(ev.Action, "contact.")) {
		s.Syncer.Kick()
	}
	w.WriteHeader(http.StatusNoContent)
}

// systemMail delivers the core's system mail through the system mail account (CC-1):
// 202 with the provider id; 503 not_configured; 422 for a bad recipient; 502 for a
// provider failure. A repeated Idempotency-Key returns the first answer and sends
// nothing.
func (s *Server) systemMail(w http.ResponseWriter, r *http.Request) {
	body, ok := s.signed(w, r)
	if !ok {
		return
	}
	var sm send.SystemMail
	if err := json.Unmarshal(body, &sm); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"code": "validation_failed", "detail": "bad JSON body"})
		return
	}
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		if sm.ID != "" && sm.ID != k {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"code": "validation_failed", "detail": "the Idempotency-Key and id differ"})
			return
		}
		sm.ID = k
	}
	res, err := s.Sender.System(r.Context(), sm)
	var inv *send.Invalid
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "sent", "provider_message_id": res.ProviderRef, "id": sm.ID})
	case errors.Is(err, send.ErrNotConfigured):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"code": "not_configured", "detail": "no connected account is marked for system mail"})
	case errors.As(err, &inv):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"code": "validation_failed", "detail": inv.Error()})
	case errors.Is(err, send.ErrFailed):
		writeJSON(w, http.StatusBadGateway, map[string]string{"code": "send_failed", "detail": err.Error()})
	default:
		s.writeErr(w, r, err)
	}
}
