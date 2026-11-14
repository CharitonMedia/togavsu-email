// Package coreapi is the typed client for the core API calls the module makes (spec
// section 6). Interactive calls carry the viewer's context token; background calls are
// service calls (no context token) or carry a task authorization. Every write carries an
// Idempotency-Key, so a filing stopped halfway resumes without duplicates.
package coreapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Module is the module id.
const Module = "email"

// Headers the core reads.
const (
	ContextHeader = "X-Togavsu-Context-Token"
	TaskHeader    = "X-Togavsu-Task-Authorization"
	IdemHeader    = "Idempotency-Key"
)

// Auth says who a call runs as: the viewer behind a context token, a task authorization,
// or (both empty) the module's service principal.
type Auth struct {
	Context string
	Task    string
}

// Service is a service call.
var Service = Auth{}

// Viewer is a call as the viewer behind tok.
func Viewer(tok string) Auth { return Auth{Context: tok} }

// Task is a call under a task authorization.
func Task(tok string) Auth { return Auth{Task: tok} }

// Error is a non-2xx core answer.
type Error struct {
	Status int
	Code   string
	Detail string
	Path   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("core %s: %d %s %s", e.Path, e.Status, e.Code, e.Detail)
}

// StatusOf returns the HTTP status of a core error, or 0.
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// IsNotFound reports a core not_found answer.
func IsNotFound(err error) bool { return StatusOf(err) == http.StatusNotFound }

// IsScopeDenied reports a core scope_denied answer.
func IsScopeDenied(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == "scope_denied"
}

// Client calls the core with the module's token.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// Call makes one request. body is JSON (or raw []byte with contentType set); out, when
// set, receives the JSON answer.
func (c *Client) Call(ctx context.Context, method, path string, a Auth, idem string, body any, out any) error {
	var rd io.Reader
	ct := ""
	switch b := body.(type) {
	case nil:
	case rawBody:
		rd, ct = bytes.NewReader(b.data), b.contentType
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return err
		}
		rd, ct = bytes.NewReader(raw), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Base, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if a.Context != "" {
		req.Header.Set(ContextHeader, a.Context)
	}
	if a.Task != "" {
		req.Header.Set(TaskHeader, a.Task)
	}
	if idem != "" {
		req.Header.Set(IdemHeader, idem)
	}
	if b, ok := body.(rawBody); ok && b.contentRange != "" {
		req.Header.Set("Content-Range", b.contentRange)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var p struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = json.Unmarshal(raw, &p)
		return &Error{Status: resp.StatusCode, Code: p.Code, Detail: p.Detail, Path: method + " " + strings.SplitN(path, "?", 2)[0]}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if w, ok := out.(*[]byte); ok {
		*w, err = io.ReadAll(resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type rawBody struct {
	data         []byte
	contentType  string
	contentRange string
}

// Viewer is the person behind a context token.
type ViewerInfo struct {
	UserID   string   `json:"viewer_user_id"`
	Roles    []string `json:"roles"`
	MatterID *string  `json:"matter_id"`
}

// Has reports whether the viewer holds any of roles.
func (v ViewerInfo) Has(roles ...string) bool {
	for _, h := range v.Roles {
		for _, w := range roles {
			if h == w {
				return true
			}
		}
	}
	return false
}

// Staff reports the admin, attorney or staff role.
func (v ViewerInfo) Staff() bool { return v.Has("admin", "attorney", "staff") && !v.Has("client") }

// VerifyContext checks a frame's or agent's context token.
func (c *Client) VerifyContext(ctx context.Context, tok string) (ViewerInfo, error) {
	var v ViewerInfo
	err := c.Call(ctx, http.MethodPost, "/api/v1/modules/"+Module+"/context-tokens/verify", Viewer(tok), "", nil, &v)
	return v, err
}

// ContextToken is a context token answer.
type ContextToken struct {
	Token        string   `json:"token"`
	ExpiresAt    string   `json:"expires_at"`
	ViewerUserID string   `json:"viewer_user_id"`
	Roles        []string `json:"roles"`
	MatterID     *string  `json:"matter_id"`
}

// ExchangeDevice trades an Outlook add-in device secret for a 10-minute context token
// (CORE-28, exchangeModuleDeviceGrant).
func (c *Client) ExchangeDevice(ctx context.Context, secret, matterID string) (ContextToken, error) {
	body := map[string]any{"device_secret": secret}
	if matterID != "" {
		body["matter_id"] = matterID
	}
	var t ContextToken
	err := c.Call(ctx, http.MethodPost, "/api/v1/modules/"+Module+"/device-context-tokens", Service, "", body, &t)
	return t, err
}

// Settings reads the module's own settings, secrets included.
func (c *Client) Settings(ctx context.Context) (map[string]string, error) {
	var rows []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := c.Call(ctx, http.MethodGet, "/api/v1/modules/"+Module+"/settings", Service, "", nil, &rows); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.Key] = r.Value
	}
	return out, nil
}

// PutSecret seals value under name in the core's module secret store (CORE-06).
func (c *Client) PutSecret(ctx context.Context, name, value string) error {
	return c.Call(ctx, http.MethodPut, "/api/v1/modules/"+Module+"/secrets/"+name, Service, "", map[string]string{"value": value}, nil)
}

// GetSecret reads a sealed secret.
func (c *Client) GetSecret(ctx context.Context, name string) (string, error) {
	var s struct {
		Value string `json:"value"`
	}
	err := c.Call(ctx, http.MethodGet, "/api/v1/modules/"+Module+"/secrets/"+name, Service, "", nil, &s)
	return s.Value, err
}

// DeleteSecret removes a sealed secret; a missing one is not an error.
func (c *Client) DeleteSecret(ctx context.Context, name string) error {
	err := c.Call(ctx, http.MethodDelete, "/api/v1/modules/"+Module+"/secrets/"+name, Service, "", nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// MatchRequest is the body of matchMail (CORE-26).
type MatchRequest struct {
	Addresses      []string `json:"addresses"`
	MatterNumbers  []string `json:"matter_numbers"`
	CaseNumbers    []string `json:"case_numbers"`
	NameTokens     []string `json:"name_tokens"`
	KnownMatterIDs []string `json:"known_matter_ids"`
}

// MatchContact is a contact holding one of the addresses.
type MatchContact struct {
	ContactID  string `json:"contact_id"`
	GivenName  string `json:"given_name"`
	FamilyName string `json:"family_name"`
}

// MatchClient is a matter's client contact.
type MatchClient struct {
	ContactID  string `json:"contact_id"`
	GivenName  string `json:"given_name"`
	FamilyName string `json:"family_name"`
}

// MatchMatter is one candidate matter.
type MatchMatter struct {
	ID           string        `json:"id"`
	Number       string        `json:"number"`
	Name         string        `json:"name"`
	Status       string        `json:"status"`
	CaseNumber   string        `json:"case_number"`
	PracticeArea string        `json:"practice_area"`
	Clients      []MatchClient `json:"clients"`
	Via          []string      `json:"via"`
}

// HasVia reports whether the matter came back for reason v.
func (m MatchMatter) HasVia(v ...string) bool {
	for _, have := range m.Via {
		for _, want := range v {
			if have == want {
				return true
			}
		}
	}
	return false
}

// MatchAnswer is matchMail's answer.
type MatchAnswer struct {
	ContactsByAddress map[string][]MatchContact `json:"contacts_by_address"`
	Matters           []MatchMatter             `json:"matters"`
	MatchToken        string                    `json:"match_token"`
}

func trim50(s []string) []string {
	if len(s) > 50 {
		return s[:50]
	}
	if s == nil {
		return []string{}
	}
	return s
}

// MatchMail asks the core for contacts and candidate matters.
func (c *Client) MatchMail(ctx context.Context, a Auth, r MatchRequest) (MatchAnswer, error) {
	r.Addresses, r.MatterNumbers, r.CaseNumbers = trim50(r.Addresses), trim50(r.MatterNumbers), trim50(r.CaseNumbers)
	r.NameTokens, r.KnownMatterIDs = trim50(r.NameTokens), trim50(r.KnownMatterIDs)
	var out MatchAnswer
	err := c.Call(ctx, http.MethodPost, "/api/v1/mail/matches", a, "", r, &out)
	return out, err
}

// FilingGrant asks for a task authorization to file one message to one matter (CORE-27).
func (c *Client) FilingGrant(ctx context.Context, matchToken, matterID, messageKey string) (string, error) {
	var out struct {
		Authorization string `json:"authorization"`
	}
	err := c.Call(ctx, http.MethodPost, "/api/v1/mail/filing-grants", Service, "",
		map[string]string{"match_token": matchToken, "matter_id": matterID, "message_key": messageKey}, &out)
	return out.Authorization, err
}

// Matter is the part of a core matter the module reads.
type Matter struct {
	ID         string                      `json:"id"`
	Number     string                      `json:"number"`
	Name       string                      `json:"name"`
	Status     string                      `json:"status"`
	CaseNumber string                      `json:"case_number"`
	Included   map[string][]map[string]any `json:"included,omitempty"`
}

// GetMatter reads one matter as a.
func (c *Client) GetMatter(ctx context.Context, a Auth, id string) (Matter, error) {
	var m Matter
	err := c.Call(ctx, http.MethodGet, "/api/v1/matters/"+url.PathEscape(id), a, "", nil, &m)
	return m, err
}

// ListMatters searches matters as a.
func (c *Client) ListMatters(ctx context.Context, a Auth, q string, limit int) ([]Matter, error) {
	var page struct {
		Data []Matter `json:"data"`
	}
	v := url.Values{"limit": {strconv.Itoa(limit)}}
	if q != "" {
		v.Set("q", q)
	}
	err := c.Call(ctx, http.MethodGet, "/api/v1/matters?"+v.Encode(), a, "", nil, &page)
	return page.Data, err
}

// Party is a matter party.
type Party struct {
	ID        string `json:"id"`
	MatterID  string `json:"matter_id"`
	ContactID string `json:"contact_id"`
	Role      string `json:"role"`
	Primary   bool   `json:"primary"`
}

// Parties lists a matter's parties as a.
func (c *Client) Parties(ctx context.Context, a Auth, matterID string) ([]Party, error) {
	var page struct {
		Data []Party `json:"data"`
	}
	err := c.Call(ctx, http.MethodGet, "/api/v1/matter-parties?matter_id="+url.QueryEscape(matterID), a, "", nil, &page)
	return page.Data, err
}

// Contact is the part of a core contact the module reads.
type Contact struct {
	ID         string `json:"id"`
	FullName   string `json:"full_name"`
	GivenName  string `json:"given_name"`
	FamilyName string `json:"family_name"`
	Archived   bool   `json:"archived"`
}

// SearchContacts searches contacts as a.
func (c *Client) SearchContacts(ctx context.Context, a Auth, q string) ([]Contact, error) {
	var page struct {
		Data []Contact `json:"data"`
	}
	err := c.Call(ctx, http.MethodGet, "/api/v1/contacts?limit=20&q="+url.QueryEscape(q), a, "", nil, &page)
	return page.Data, err
}

// GetContact reads a contact as a.
func (c *Client) GetContact(ctx context.Context, a Auth, id string) (Contact, error) {
	var ct Contact
	err := c.Call(ctx, http.MethodGet, "/api/v1/contacts/"+url.PathEscape(id), a, "", nil, &ct)
	return ct, err
}

// ContactEmail is one of a contact's addresses.
type ContactEmail struct {
	ContactID   string  `json:"contact_id"`
	Email       string  `json:"email"`
	Primary     bool    `json:"primary"`
	EffectiveTo *string `json:"effective_to"`
}

// ContactEmails lists a contact's addresses as a.
func (c *Client) ContactEmails(ctx context.Context, a Auth, contactID string) ([]ContactEmail, error) {
	var page struct {
		Data []ContactEmail `json:"data"`
	}
	err := c.Call(ctx, http.MethodGet, "/api/v1/contact-emails?contact_id="+url.QueryEscape(contactID), a, "", nil, &page)
	return page.Data, err
}

// Upload is a file upload session.
type Upload struct {
	ID             string `json:"id"`
	ChunkSizeBytes int64  `json:"chunk_size_bytes"`
	NextOffset     int64  `json:"next_offset"`
	SizeBytes      int64  `json:"size_bytes"`
	Status         string `json:"status"`
}

// File is the part of a core file the module reads.
type File struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	MatterID         *string `json:"matter_id"`
	Origin           string  `json:"origin"`
	AttorneyOnly     bool    `json:"attorney_only"`
	CurrentVersionID *string `json:"current_version_id"`
	CreatedBy        string  `json:"created_by"`
}

// Version is a file version.
type Version struct {
	ID        string `json:"id"`
	FileID    string `json:"file_id"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type"`
}

// PublishFile uploads data and publishes it as a file on matterID (createFileUpload,
// putFileChunk, completeFileUpload, publishFile), every step under key-derived
// idempotency keys so a replay returns the same file.
func (c *Client) PublishFile(ctx context.Context, a Auth, key, matterID, name, mediaType, sha string, data []byte, origin string, attorneyOnly bool) (File, error) {
	var up Upload
	if err := c.Call(ctx, http.MethodPost, "/api/v1/file-uploads", a, key+"-upload",
		map[string]any{"file_name": name, "matter_id": matterID, "media_type": mediaType, "size_bytes": len(data), "sha256": sha}, &up); err != nil {
		return File{}, err
	}
	chunk := up.ChunkSizeBytes
	if chunk <= 0 {
		chunk = 8 << 20
	}
	for off := int64(0); off < int64(len(data)); off += chunk {
		end := off + chunk
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		rng := fmt.Sprintf("bytes %d-%d/%d", off, end-1, len(data))
		if err := c.Call(ctx, http.MethodPut, "/api/v1/file-uploads/"+up.ID, a, fmt.Sprintf("%s-chunk-%d", key, off),
			rawBody{data: data[off:end], contentType: "application/octet-stream", contentRange: rng}, &up); err != nil {
			return File{}, err
		}
	}
	if err := c.Call(ctx, http.MethodPost, "/api/v1/file-uploads/"+up.ID+"/complete", a, key+"-complete", nil, &up); err != nil {
		return File{}, err
	}
	var f File
	err := c.Call(ctx, http.MethodPost, "/api/v1/files", a, key+"-publish",
		map[string]any{"upload_id": up.ID, "matter_id": matterID, "name": name, "attorney_only": attorneyOnly, "origin": origin}, &f)
	return f, err
}

// LinkAttachment links the HTML copy to one attachment.
func (c *Client) LinkAttachment(ctx context.Context, a Auth, key, fromFile, toFile string) error {
	return c.Call(ctx, http.MethodPost, "/api/v1/files/"+url.PathEscape(fromFile)+"/links", a, key,
		map[string]string{"to_file_id": toFile, "relationship": "attachment"}, nil)
}

// GetFile reads a file as a.
func (c *Client) GetFile(ctx context.Context, a Auth, id string) (File, error) {
	var f File
	err := c.Call(ctx, http.MethodGet, "/api/v1/files/"+url.PathEscape(id), a, "", nil, &f)
	return f, err
}

// ListFiles lists a matter's files as a.
func (c *Client) ListFiles(ctx context.Context, a Auth, matterID string) ([]File, error) {
	var page struct {
		Data []File `json:"data"`
	}
	err := c.Call(ctx, http.MethodGet, "/api/v1/files?limit=100&matter_id="+url.QueryEscape(matterID), a, "", nil, &page)
	return page.Data, err
}

// Versions lists a file's versions as a.
func (c *Client) Versions(ctx context.Context, a Auth, fileID string) ([]Version, error) {
	var page struct {
		Data []Version `json:"data"`
	}
	err := c.Call(ctx, http.MethodGet, "/api/v1/files/"+url.PathEscape(fileID)+"/versions", a, "", nil, &page)
	return page.Data, err
}

// Download reads one version's bytes as a.
func (c *Client) Download(ctx context.Context, a Auth, fileID, versionID string) ([]byte, error) {
	var b []byte
	err := c.Call(ctx, http.MethodGet, "/api/v1/files/"+url.PathEscape(fileID)+"/versions/"+url.PathEscape(versionID)+"/download", a, "", nil, &b)
	return b, err
}

// Note is a core note.
type Note struct {
	ID string `json:"id"`
}

// CreateNote writes one note on a matter.
func (c *Client) CreateNote(ctx context.Context, a Auth, key, matterID, title, body string, tags []string) (Note, error) {
	var n Note
	err := c.Call(ctx, http.MethodPost, "/api/v1/notes", a, key,
		map[string]any{"matter_id": matterID, "title": title, "body": body, "tags": tags}, &n)
	return n, err
}

// Notify sends one notification to one user (CORE-03), as a service call.
func (c *Client) Notify(ctx context.Context, key, userID, kind, title, link string) error {
	return c.Call(ctx, http.MethodPost, "/api/v1/notifications", Service, key,
		map[string]string{"user_id": userID, "kind": kind, "title": title, "link": link}, nil)
}

// AICall runs one of the module's AI tasks. With a match token it runs as the
// mail_matching workflow (CORE-27).
func (c *Client) AICall(ctx context.Context, a Auth, task string, input any, out any) error {
	var call struct {
		Output json.RawMessage `json:"output"`
	}
	if err := c.Call(ctx, http.MethodPost, "/api/v1/ai/calls", a, "", map[string]any{"task": task, "input": input}, &call); err != nil {
		return err
	}
	if len(call.Output) == 0 {
		return errors.New("core: AI call returned no output")
	}
	return json.Unmarshal(call.Output, out)
}
