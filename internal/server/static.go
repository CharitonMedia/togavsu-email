package server

import (
	"net/http"
)

// app.js: the frames' small script. Pages work as plain forms without it; it adds the
// Defer all confirmation and opens OAuth consent pages in their own window.
const appScript = `(function () {
  "use strict";
  document.addEventListener("submit", function (e) {
    var f = e.target;
    if (f.dataset.confirm && !window.confirm(f.dataset.confirm)) { e.preventDefault(); return; }
    if (f.hasAttribute("data-oauth")) {
      e.preventDefault();
      var body = {}; new FormData(f).forEach(function (v, k) { body[k] = v; });
      if (e.submitter && e.submitter.name) { body[e.submitter.name] = e.submitter.value; }
      start(body);
    }
  });
  document.addEventListener("click", function (e) {
    var id = e.target.getAttribute && e.target.getAttribute("data-oauth-reconnect");
    if (id) { start({ account_id: id }); }
  });
  function start(body) {
    var ctx = new URLSearchParams(window.location.search).get("context") || "";
    fetch(window.location.pathname.replace(/\/[^\/]*$/, "") + "/api/oauth/start?context=" + encodeURIComponent(ctx), {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body)
    }).then(function (r) { return r.json(); }).then(function (res) {
      if (res.url) { window.open(res.url, "_blank", "noopener"); } else { window.alert(res.detail || "The sign-in could not start."); }
    });
  }
})();
`

func appJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(appScript))
}

// openAPIDoc describes the module's own API for the add-in and integrators.
const openAPIDoc = `{
 "openapi": "3.1.0",
 "info": {"title": "Togavsu Email module", "version": "1.0.0"},
 "components": {"securitySchemes": {
   "context": {"type": "apiKey", "in": "query", "name": "context", "description": "The context token the core hands each frame"},
   "device": {"type": "apiKey", "in": "header", "name": "X-Togavsu-Device-Grant", "description": "The Outlook add-in's device grant (CORE-28)"},
   "signature": {"type": "apiKey", "in": "header", "name": "X-Togavsu-Signature", "description": "The core's webhook signature"}}},
 "paths": {
  "/api/queue": {"get": {"summary": "List unfiled email the viewer may see", "security": [{"context": []}]}},
  "/api/queue/{id}": {"get": {"summary": "One queue row with its suggestions", "security": [{"context": []}]}},
  "/api/queue/{id}/file": {"post": {"summary": "File to a matter as the viewer", "security": [{"context": []}]}},
  "/api/queue/{id}/defer": {"post": {"summary": "Move to Review-Later", "security": [{"context": []}]}},
  "/api/queue/{id}/restore": {"post": {"summary": "Return a deferred row to the queue", "security": [{"context": []}]}},
  "/api/queue/{id}/dismiss": {"post": {"summary": "Dismiss with a reason", "security": [{"context": []}]}},
  "/api/queue/{id}/retry": {"post": {"summary": "Read an error row again", "security": [{"context": []}]}},
  "/api/queue/defer-all": {"post": {"summary": "Defer every row in view once confirm_count names their count", "security": [{"context": []}]}},
  "/api/send": {"post": {"summary": "Send a quick email, reply or forward", "security": [{"context": []}]}},
  "/api/search": {"get": {"summary": "Search email", "security": [{"context": []}]}},
  "/api/inbox": {"get": {"summary": "The viewer's Inbox or Sent", "security": [{"context": []}]}},
  "/api/messages/{id}": {"get": {"summary": "One message", "security": [{"context": []}]}},
  "/api/matters/{id}/mail": {"get": {"summary": "A matter's filed mail by thread", "security": [{"context": []}]}},
  "/api/accounts": {"get": {"summary": "Accounts the viewer manages", "security": [{"context": []}]}, "post": {"summary": "Connect an IMAP/SMTP account", "security": [{"context": []}]}},
  "/api/oauth/start": {"post": {"summary": "Start a Gmail or Microsoft consent", "security": [{"context": []}]}},
  "/badge/unfiled": {"get": {"summary": "The sidebar count", "security": [{"context": []}]}},
  "/addin/api/ping": {"get": {"summary": "Who the add-in is signed in as", "security": [{"device": []}]}},
  "/addin/api/suggest": {"post": {"summary": "Suggest the matter for a message's addresses", "security": [{"device": []}]}},
  "/addin/api/matters": {"get": {"summary": "Matter search", "security": [{"device": []}]}},
  "/addin/api/contacts": {"get": {"summary": "Contact search", "security": [{"device": []}]}},
  "/addin/api/file": {"post": {"summary": "File the open item", "security": [{"device": []}]}},
  "/addin/api/plus-address": {"get": {"summary": "The filing plus address for a matter", "security": [{"device": []}]}},
  "/events": {"post": {"summary": "The core's signed event webhooks", "security": [{"signature": []}]}},
  "/system-mail": {"post": {"summary": "The core's signed system mail (CC-1)", "security": [{"signature": []}]}}
 }
}`

func openAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(openAPIDoc))
}
