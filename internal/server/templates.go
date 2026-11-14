package server

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"
)

// PageCSP is the frames' policy: their own script and forms, nothing remote.
const PageCSP = "default-src 'none'; style-src 'unsafe-inline'; script-src 'self'; img-src data:; frame-src 'self'; form-action 'self'; base-uri 'none'"

var funcs = template.FuncMap{
	"when": func(ms int64) string {
		if ms == 0 {
			return ""
		}
		return time.UnixMilli(ms).UTC().Format("Jan 2, 2006 15:04 UTC")
	},
	"whenp": func(ms *int64) string {
		if ms == nil {
			return ""
		}
		return time.UnixMilli(*ms).UTC().Format("Jan 2, 2006 15:04 UTC")
	},
	"age": func(ms int64) string {
		d := time.Since(time.UnixMilli(ms))
		switch {
		case d < time.Hour:
			return "under an hour"
		case d < 48*time.Hour:
			return fmt.Sprintf("%d hours", int(d.Hours()))
		}
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	},
	"pair": func(p any, id string) pairData { return pairData{P: p, ID: id} },
	"label": func(path string) string {
		_, act, _ := strings.Cut(path, "/")
		return map[string]string{"pause": "Pause", "resume": "Resume", "sync": "Sync now", "default-send": "Default send",
			"system-mail": "System mail", "remove": "Remove"}[act]
	},
	"join": strings.Join,
	"m":    func(path string) string { return ModulePrefix + path },
}

// layout wraps every frame page.
const layout = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
body{font:14px/1.45 system-ui,sans-serif;margin:0;padding:12px 16px;color:#1d2330;background:#fff}
h1{font-size:18px;margin:0 0 12px}h2{font-size:15px;margin:18px 0 8px}
table{border-collapse:collapse;width:100%}th,td{text-align:left;padding:6px 8px;border-bottom:1px solid #e3e6ec;vertical-align:top}
th{font-weight:600;color:#525a6b;font-size:12px;text-transform:uppercase;letter-spacing:.03em}
nav a{margin-right:12px}a{color:#1f5fbf}.muted{color:#6b7385}.warn{color:#a1460a}.ok{color:#22703a}
form.inline{display:inline}button{font:inherit;padding:3px 10px;border:1px solid #b9c0cd;border-radius:4px;background:#f6f7f9;cursor:pointer}
button.primary{background:#1f5fbf;color:#fff;border-color:#1f5fbf}input[type=text],input[type=email],input[type=password],input[type=number],textarea,select{font:inherit;padding:4px 6px;border:1px solid #b9c0cd;border-radius:4px}
textarea{width:100%;min-height:180px}.grid{display:grid;grid-template-columns:1fr 1fr;gap:16px}.field{margin:6px 0}.field label{display:inline-block;min-width:110px}
iframe.preview{width:100%;height:70vh;border:1px solid #e3e6ec}.badge{display:inline-block;padding:0 6px;border-radius:9px;background:#eef1f6;font-size:12px}
</style></head><body>
{{template "body" .}}
<script src="` + ModulePrefix + `/static/app.js"></script>
</body></html>`

func parse(body string) *template.Template {
	return template.Must(template.Must(template.New("page").Funcs(funcs).Parse(layout)).New("body").Parse(body))
}

// writePage renders a page from a named template.
func renderPage(w http.ResponseWriter, status int, t *template.Template, data any) error {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "page", data); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", PageCSP)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
	return nil
}

var messageTpl = parse(`<h1>{{.Title}}</h1><p>{{.Text}}</p>`)

type messageData struct{ Title, Text string }

func messageBody(text string) messageData { return messageData{Text: text} }

func writePage(w http.ResponseWriter, status int, title string, d messageData) {
	d.Title = title
	_ = renderPage(w, status, messageTpl, d)
}

const nav = `{{define "nav"}}<nav><a href="{{m "/inbox"}}?context={{.Ctx}}">Mail</a><a href="{{m "/unfiled"}}?context={{.Ctx}}">Unfiled email</a>` +
	`<a href="{{m "/compose"}}?context={{.Ctx}}">New email</a><a href="{{m "/accounts"}}?context={{.Ctx}}">Mail accounts</a>` +
	`<form class="inline" method="get" action="{{m "/search"}}"><input type="hidden" name="context" value="{{.Ctx}}"><input type="text" name="q" placeholder="Search email" aria-label="Search email"></form></nav>{{end}}`

func parseNav(body string) *template.Template { return parse(nav + body) }

var inboxTpl = parseNav(`{{template "nav" .}}
<h1>{{if eq .Folder "sent"}}Sent{{else}}Inbox{{end}}</h1>
<p><a href="{{m "/inbox"}}?context={{.Ctx}}&folder=inbox">Inbox</a> · <a href="{{m "/inbox"}}?context={{.Ctx}}&folder=sent">Sent</a>
{{if .Unread}} · <span class="badge">{{.Unread}} unread</span>{{end}}</p>
{{if not .HasAccounts}}<p>You have no mail account connected. <a href="{{m "/accounts"}}?context={{.Ctx}}">Connect one</a>.</p>
{{else if .Rows}}<table><thead><tr><th>From</th><th>Subject</th><th>Received</th><th>Status</th></tr></thead><tbody>
{{range .Rows}}<tr data-id="{{.ID}}"><td>{{if .Unread}}<strong>{{.FromAddr}}</strong>{{else}}{{.FromAddr}}{{end}}</td>
<td><a href="{{m "/messages/"}}{{.ID}}?context={{$.Ctx}}">{{if .Subject}}{{.Subject}}{{else}}(no subject){{end}}</a><div class="muted">{{.Snippet}}</div></td>
<td>{{when .ReceivedAt}}</td><td>{{.Status}}</td></tr>{{end}}</tbody></table>
{{else}}<p class="muted">Nothing here yet.</p>{{end}}`)

var messageViewTpl = parseNav(`{{template "nav" .}}
<h1>{{if .M.Subject}}{{.M.Subject}}{{else}}(no subject){{end}}</h1>
<table><tr><th>From</th><td>{{.M.FromHeader}}</td></tr><tr><th>To</th><td>{{join .M.ToAddrs ", "}}</td></tr>
{{if .M.CcAddrs}}<tr><th>Cc</th><td>{{join .M.CcAddrs ", "}}</td></tr>{{end}}<tr><th>Date</th><td>{{whenp .M.SentAt}}</td></tr>
<tr><th>Status</th><td>{{.M.Status}}{{if .Matter}} to {{.Matter}}{{end}}</td></tr></table>
<p>{{if .CanAct}}<a href="{{m "/compose"}}?context={{.Ctx}}&reply_to={{.M.ID}}&kind=reply">Reply</a> ·
<a href="{{m "/compose"}}?context={{.Ctx}}&reply_to={{.M.ID}}&kind=reply&all=1">Reply all</a> ·
<a href="{{m "/compose"}}?context={{.Ctx}}&reply_to={{.M.ID}}&kind=forward">Forward</a>
{{if ne .M.Status "filed"}} · <a href="{{m "/file/"}}{{.M.ID}}?context={{.Ctx}}">File to a matter</a>{{end}}
<form class="inline" method="post" action="{{m "/api/messages/"}}{{.M.ID}}/read"><input type="hidden" name="context" value="{{.Ctx}}">
<input type="hidden" name="read" value="{{if .M.Unread}}true{{else}}false{{end}}"><input type="hidden" name="back" value="/messages/{{.M.ID}}">
<button>{{if .M.Unread}}Mark read{{else}}Mark unread{{end}}</button></form>{{end}}</p>
<iframe class="preview" sandbox title="Message" src="{{m "/preview/"}}{{.M.ID}}?context={{.Ctx}}"></iframe>`)

var unfiledTpl = parseNav(`{{template "nav" .}}
<h1>Unfiled email</h1>
<p><a href="{{m "/unfiled"}}?context={{.Ctx}}">Waiting</a> · <a href="{{m "/unfiled"}}?context={{.Ctx}}&filter=deferred">Deferred</a>
{{if .AccountID}} · filtered to one account (<a href="{{m "/unfiled"}}?context={{.Ctx}}">show all</a>){{end}}</p>
{{if .Rows}}
{{if ne .Filter "deferred"}}<form method="post" action="{{m "/api/queue/defer-all"}}" data-confirm="Defer all {{len .Rows}} messages in view?">
<input type="hidden" name="context" value="{{.Ctx}}"><input type="hidden" name="account_id" value="{{.AccountID}}">
<input type="hidden" name="confirm_count" value="{{len .Rows}}"><button>Defer all {{len .Rows}}</button></form>{{end}}
<table><thead><tr><th>Account</th><th>From</th><th>Subject</th><th>Received</th><th>Files</th><th>Why</th><th>Suggestion</th><th></th></tr></thead><tbody>
{{range .Rows}}<tr data-id="{{.ID}}"><td>{{.Account}}</td><td>{{.From}}</td><td>{{.Subject}}</td><td>{{when .ReceivedAt}}</td><td>{{.Attachments}}</td>
<td>{{.Reason}}{{if .Error}}<div class="warn">{{.Error}}</div>{{end}}</td>
<td>{{range .Suggestions}}{{.Number}} {{.Name}}{{end}}</td><td>
{{if eq .Status "deferred"}}<form class="inline" method="post" action="{{m "/api/queue/"}}{{.ID}}/restore"><input type="hidden" name="context" value="{{$.Ctx}}"><button>Restore</button></form>
{{else}}<a href="{{m "/file/"}}{{.ID}}?context={{$.Ctx}}">File</a>
<form class="inline" method="post" action="{{m "/api/queue/"}}{{.ID}}/defer"><input type="hidden" name="context" value="{{$.Ctx}}"><button>Defer</button></form>
{{if eq .Status "error"}}<form class="inline" method="post" action="{{m "/api/queue/"}}{{.ID}}/retry"><input type="hidden" name="context" value="{{$.Ctx}}"><button>Retry</button></form>{{end}}
<form class="inline" method="post" action="{{m "/api/queue/"}}{{.ID}}/dismiss"><input type="hidden" name="context" value="{{$.Ctx}}">
<input type="text" name="reason" required placeholder="Reason" aria-label="Reason to dismiss"><button>Dismiss</button></form>{{end}}
</td></tr>{{end}}</tbody></table>
{{else}}<p class="muted">Nothing waits to be filed.</p>{{end}}`)

var fileTpl = parseNav(`{{template "nav" .}}
<h1>File: {{if .M.Subject}}{{.M.Subject}}{{else}}(no subject){{end}}</h1>
<div class="grid"><div><iframe class="preview" sandbox title="Preview" src="{{m "/preview/"}}{{.M.ID}}?context={{.Ctx}}"></iframe></div><div>
{{if .Row.FileWithoutAttachments}}<p class="warn">The message is gone from the mailbox. It can be filed without its attachments.</p>{{end}}
<h2>Suggested matters</h2>{{if .Row.Suggestions}}<table>{{range .Row.Suggestions}}<tr><td>{{.Number}}</td><td>{{.Name}}</td><td>{{.Status}}</td><td>{{template "fileform" (pair $ .MatterID)}}</td></tr>{{end}}</table>
{{else}}<p class="muted">No suggestion.</p>{{end}}
<h2>Find a matter</h2><form method="get" action="{{m "/file/"}}{{.M.ID}}"><input type="hidden" name="context" value="{{.Ctx}}">
<input type="text" name="q" value="{{.Q}}" placeholder="Matter name, number or client" aria-label="Find a matter"><button>Search</button></form>
{{if .Found}}<table>{{range .Found}}<tr><td>{{.Number}}</td><td>{{.Name}}</td><td>{{.Status}}</td><td>{{template "fileform" (pair $ .ID)}}</td></tr>{{end}}</table>{{end}}
</div></div>
{{define "fileform"}}<form class="inline" method="post" action="{{m "/api/queue/"}}{{.P.M.ID}}/file"><input type="hidden" name="context" value="{{.P.Ctx}}">
<input type="hidden" name="matter_id" value="{{.ID}}">{{if .P.Row.FileWithoutAttachments}}<input type="hidden" name="without_attachments" value="true">{{end}}
{{if .P.Lawyer}}<label><input type="checkbox" name="attorney_only" value="true"> Attorney only</label>{{end}}
<button class="primary">{{if .P.Row.FileWithoutAttachments}}File without attachments{{else}}File{{end}}</button></form>{{end}}`)

var accountsTpl = parseNav(`{{template "nav" .}}
<h1>Mail accounts</h1>
{{if .Accounts}}<table><thead><tr><th>Address</th><th>Role</th><th>Provider</th><th>Status</th><th>Last sync</th><th></th></tr></thead><tbody>
{{range .Accounts}}<tr data-id="{{.ID}}"><td>{{.Address}}{{if .Label}} <span class="muted">{{.Label}}</span>{{end}}{{if .DefaultSend}} <span class="badge">default send</span>{{end}}{{if .SystemMail}} <span class="badge">system mail</span>{{end}}</td>
<td>{{.Role}}</td><td>{{.Provider}}</td><td>{{if eq .Status "connected"}}<span class="ok">Connected</span>{{else}}<span class="warn">{{.Status}}</span>{{end}}{{if .LastError}}<div class="muted">{{.LastError}}</div>{{end}}</td>
<td>{{whenp .LastSyncAt}}</td><td>
{{if eq .Status "paused"}}{{template "act" (pair $ (print .ID "/resume"))}}{{else}}{{template "act" (pair $ (print .ID "/pause"))}}{{end}}
{{template "act" (pair $ (print .ID "/sync"))}}
{{if $.Admin}}{{if .CanSend}}{{template "act" (pair $ (print .ID "/default-send"))}}{{template "act" (pair $ (print .ID "/system-mail"))}}{{end}}{{end}}
{{if or (eq .Status "needs_reauth") (ne .Provider "imap")}}<button data-oauth-reconnect="{{.ID}}">Reconnect</button>{{end}}
{{template "act" (pair $ (print .ID "/remove"))}}</td></tr>{{end}}</tbody></table>
{{else}}<p class="muted">No account is connected.</p>{{end}}
<h2>Connect an account</h2>
<form method="post" action="{{m "/api/oauth/start"}}" data-oauth><input type="hidden" name="context" value="{{.Ctx}}">
<div class="field"><label>Role</label><select name="role">{{if .Admin}}<option value="filing">Filing</option><option value="court">Court</option>{{end}}<option value="personal" selected>Personal</option></select></div>
<div class="field"><label>Label</label><input type="text" name="label"></div>
<button name="provider" value="gmail">Connect Gmail</button> <button name="provider" value="microsoft">Connect Microsoft</button></form>
<h2>Connect IMAP/SMTP</h2>
<form method="post" action="{{m "/api/accounts"}}" autocomplete="off"><input type="hidden" name="context" value="{{.Ctx}}">
<div class="field"><label>Role</label><select name="role">{{if .Admin}}<option value="filing">Filing</option><option value="court">Court</option>{{end}}<option value="personal" selected>Personal</option></select></div>
{{if .Admin}}<div class="field"><label>Owner user id</label><input type="text" name="owner_user_id" placeholder="(you)"></div>{{end}}
<div class="field"><label>Address</label><input type="email" name="address" required></div>
<div class="field"><label>Label</label><input type="text" name="label"></div>
<div class="field"><label>IMAP host</label><input type="text" name="imap_host" required> <input type="number" name="imap_port" value="993" aria-label="IMAP port">
<select name="imap_security" aria-label="IMAP security"><option>tls</option><option>starttls</option></select></div>
<div class="field"><label>SMTP host</label><input type="text" name="smtp_host" required> <input type="number" name="smtp_port" value="587" aria-label="SMTP port">
<select name="smtp_security" aria-label="SMTP security"><option>starttls</option><option>tls</option></select></div>
<div class="field"><label>Login</label><input type="text" name="login" required></div>
<div class="field"><label>Password</label><input type="password" name="password" required autocomplete="new-password"></div>
<button class="primary">Connect</button></form>
<h2>Your notices</h2>
<form method="post" action="{{m "/api/prefs"}}"><input type="hidden" name="context" value="{{.Ctx}}">
<input type="hidden" name="notify_unfiled" value="false"><label><input type="checkbox" name="notify_unfiled" value="true" {{if .Prefs.NotifyUnfiled}}checked{{end}}> Tell me about every new unfiled email</label>
<div class="field"><label>Signature</label><textarea name="signature_text" style="min-height:60px">{{.Prefs.SignatureText}}</textarea></div><button>Save</button></form>
{{if .Admin}}<h2>Outlook add-in</h2><p><a href="{{m "/outlook-addin/manifest.xml"}}?context={{.Ctx}}" download="manifest.xml">Download manifest.xml</a> and deploy it to the firm's Microsoft 365 tenant, or sideload it in Outlook.</p>{{end}}
{{define "act"}}<form class="inline" method="post" action="{{m "/api/accounts/"}}{{.ID}}"><input type="hidden" name="context" value="{{.P.Ctx}}"><button>{{label .ID}}</button></form>{{end}}`)

var matterTpl = parse(`<h1>Mail</h1>
{{if .Threads}}{{range .Threads}}<h2>{{.Subject}}</h2><table>{{range .Filings}}<tr><td>{{whenp .SentAt}}</td><td>{{.Direction}}</td><td>{{.FromAddr}}</td>
<td><a href="{{m "/messages/"}}{{.MessageID}}?context={{$.Ctx}}">{{.Subject}}</a></td><td>{{.How}}</td></tr>{{end}}</table>{{end}}
{{else}}<p class="muted">No mail is filed to this matter yet.</p>{{end}}
<p><a href="{{m "/compose"}}?context={{.Ctx}}&matter_id={{.MatterID}}">Email the client</a></p>`)

var recentTpl = parse(`{{if .Filings}}<table>{{range .Filings}}<tr><td>{{whenp .SentAt}}</td><td>{{.FromAddr}}</td>
<td><a href="{{m "/messages/"}}{{.MessageID}}?context={{$.Ctx}}">{{.Subject}}</a></td></tr>{{end}}</table>{{else}}<p class="muted">No mail yet.</p>{{end}}`)

var composeTpl = parseNav(`{{template "nav" .}}
<h1>{{.Heading}}</h1>
{{if .Error}}<p class="warn">{{.Error}}</p>{{end}}
<form method="post" action="{{m "/api/send"}}"><input type="hidden" name="context" value="{{.Ctx}}">
<input type="hidden" name="kind" value="{{.Kind}}"><input type="hidden" name="reply_to" value="{{.ReplyTo}}"><input type="hidden" name="idempotency_key" value="{{.Key}}">
<input type="hidden" name="back" value="/compose?sent=1">
{{if .Accounts}}<div class="field"><label>From</label><select name="account_id"><option value="">(automatic)</option>{{range .Accounts}}<option value="{{.ID}}">{{.Address}}</option>{{end}}</select></div>{{end}}
<div class="field"><label>To</label><input type="text" name="to" value="{{.To}}" size="60" required></div>
<div class="field"><label>Cc</label><input type="text" name="cc" value="{{.Cc}}" size="60"></div>
<div class="field"><label>Bcc</label><input type="text" name="bcc" size="60"></div>
<div class="field"><label>Subject</label><input type="text" name="subject" value="{{.Subject}}" size="60"></div>
<div class="field"><label>Matter</label>{{if .Matter}}<input type="hidden" name="matter_id" value="{{.Matter.ID}}">{{.Matter.Number}} {{.Matter.Name}}{{else}}<input type="text" name="matter_id" placeholder="Matter id (optional)">{{end}}</div>
{{if .Files}}<div class="field"><label>Attach</label>{{range .Files}}<label><input type="checkbox" name="file_ids[]" value="{{.ID}}"> {{.Name}}</label> {{end}}</div>{{end}}
<textarea name="body" required aria-label="Message">{{.Body}}</textarea>
<p><button class="primary">Send</button></p></form>`)

var widgetInboxTpl = parse(`<p><strong>{{.Unread}}</strong> unread{{if .Unfiled}} · <strong>{{.Unfiled}}</strong> unfiled you can act on{{end}}</p>
{{if .Rows}}<table>{{range .Rows}}<tr><td>{{if .Unread}}<strong>{{.FromAddr}}</strong>{{else}}{{.FromAddr}}{{end}}</td><td>{{.Subject}}</td><td class="muted">{{when .ReceivedAt}}</td></tr>{{end}}</table>
{{else}}<p class="muted">{{if .HasAccounts}}No mail.{{else}}No mail account connected.{{end}}</p>{{end}}`)

var widgetUnfiledTpl = parse(`{{if .Rows}}<table>{{range .Rows}}<tr><td>{{.From}}</td><td>{{.Subject}}</td><td class="muted">{{age .ReceivedAt}}</td>
<td>{{range .Suggestions}}{{.Number}}{{end}}</td></tr>{{end}}</table>{{else}}<p class="muted">Nothing waits to be filed.</p>{{end}}`)

var searchTpl = parseNav(`{{template "nav" .}}
<h1>Search email</h1>
{{if .Q}}{{if .Rows}}<table>{{range .Rows}}<tr><td>{{.FromAddr}}</td><td><a href="{{m "/messages/"}}{{.ID}}?context={{$.Ctx}}">{{.Subject}}</a><div class="muted">{{.Snippet}}</div></td><td>{{when .ReceivedAt}}</td><td>{{.Status}}</td></tr>{{end}}</table>
{{else}}<p class="muted">No email matches.</p>{{end}}{{end}}`)

// pair passes the page data and one value into a sub-template.
type pairData struct {
	P  any
	ID string
}
