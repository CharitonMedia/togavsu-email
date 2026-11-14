// Package gmail is the adapter for Gmail accounts over the Gmail REST API. Folders
// are the INBOX and SENT labels plus user labels; a ref is the label id and the
// message id.
package gmail

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/oauth"
	"github.com/CharitonMedia/togavsu-email/internal/provider"
	"github.com/CharitonMedia/togavsu-email/internal/provider/restapi"
)

// Adapter is one Gmail account.
type Adapter struct {
	API  *restapi.Client
	base string

	mu     sync.Mutex
	labels map[string]string // name -> id
}

// New returns the adapter for c. c.APIBase and c.TokenURL override Google's
// endpoints (tests point them at httptest servers on 127.0.0.1).
func New(c provider.Config, hc *http.Client) *Adapter {
	tokenURL := c.TokenURL
	if tokenURL == "" {
		tokenURL = oauth.For("gmail", "").TokenURL
	}
	base := c.APIBase
	if base == "" {
		base = oauth.GmailAPI
	}
	return &Adapter{API: restapi.New(hc, c, tokenURL), base: strings.TrimRight(base, "/") + "/gmail/v1/users/me"}
}

func (a *Adapter) get(ctx context.Context, path string, q url.Values, out any) error {
	u := a.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	_, err := a.API.Do(ctx, restapi.Request{Method: http.MethodGet, URL: u}, out)
	return err
}

func (a *Adapter) post(ctx context.Context, path string, body, out any) error {
	_, err := a.API.Do(ctx, restapi.Request{Method: http.MethodPost, URL: a.base + path, JSON: body}, out)
	return err
}

type label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func (a *Adapter) labelList(ctx context.Context) ([]label, error) {
	var res struct {
		Labels []label `json:"labels"`
	}
	if err := a.get(ctx, "/labels", nil, &res); err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.labels = map[string]string{}
	for _, l := range res.Labels {
		a.labels[strings.ToLower(l.Name)] = l.ID
	}
	a.mu.Unlock()
	return res.Labels, nil
}

// labelID finds the label by name, creating it when missing.
func (a *Adapter) labelID(ctx context.Context, name string) (string, error) {
	switch strings.ToUpper(name) {
	case "INBOX", "SENT", "TRASH", "UNREAD":
		return strings.ToUpper(name), nil
	}
	a.mu.Lock()
	id, ok := a.labels[strings.ToLower(name)]
	loaded := a.labels != nil
	a.mu.Unlock()
	if ok {
		return id, nil
	}
	if !loaded {
		if _, err := a.labelList(ctx); err != nil {
			return "", err
		}
		a.mu.Lock()
		id, ok = a.labels[strings.ToLower(name)]
		a.mu.Unlock()
		if ok {
			return id, nil
		}
	}
	var l label
	err := a.post(ctx, "/labels", map[string]string{"name": name, "labelListVisibility": "labelShow", "messageListVisibility": "show"}, &l)
	if restapi.StatusOf(err) == http.StatusConflict {
		a.mu.Lock()
		a.labels = nil
		a.mu.Unlock()
		return a.labelID(ctx, name)
	}
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.labels[strings.ToLower(name)] = l.ID
	a.mu.Unlock()
	return l.ID, nil
}

// Check implements provider.Provider.
func (a *Adapter) Check(ctx context.Context) error {
	var p struct {
		EmailAddress string `json:"emailAddress"`
	}
	return a.get(ctx, "/profile", nil, &p)
}

// Folders implements provider.Provider: INBOX, SENT and the user labels.
func (a *Adapter) Folders(ctx context.Context) ([]provider.Folder, error) {
	ls, err := a.labelList(ctx)
	if err != nil {
		return nil, err
	}
	var out []provider.Folder
	for _, l := range ls {
		switch {
		case l.ID == "INBOX":
			out = append(out, provider.Folder{Ref: l.ID, Name: "INBOX", Kind: provider.KindInbox})
		case l.ID == "SENT":
			out = append(out, provider.Folder{Ref: l.ID, Name: "SENT", Kind: provider.KindSent})
		case l.Type == "user":
			out = append(out, provider.Folder{Ref: l.ID, Name: l.Name, Kind: provider.KindFor(l.Name, "")})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

func (a *Adapter) historyID(ctx context.Context) (string, error) {
	var p struct {
		HistoryID string `json:"historyId"`
	}
	err := a.get(ctx, "/profile", nil, &p)
	return p.HistoryID, err
}

// list reads every message id carrying the label since the given time, oldest first.
func (a *Adapter) list(ctx context.Context, labelID string, since time.Time) ([]string, error) {
	q := url.Values{"labelIds": {labelID}, "maxResults": {"500"}}
	if !since.IsZero() {
		q.Set("q", "after:"+strconv.FormatInt(since.Unix(), 10))
	}
	var ids []string
	for {
		var res struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := a.get(ctx, "/messages", q, &res); err != nil {
			return nil, err
		}
		for _, m := range res.Messages {
			ids = append(ids, m.ID)
		}
		if res.NextPageToken == "" {
			break
		}
		q.Set("pageToken", res.NextPageToken)
	}
	// The list is newest first.
	for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
		ids[i], ids[j] = ids[j], ids[i]
	}
	return ids, nil
}

// history reads the messages added to the label since the history id.
func (a *Adapter) history(ctx context.Context, labelID, start string) ([]string, string, error) {
	q := url.Values{"startHistoryId": {start}, "labelId": {labelID}, "historyTypes": {"messageAdded", "labelAdded"}, "maxResults": {"500"}}
	var ids []string
	seen := map[string]bool{}
	last := start
	for {
		var res struct {
			History []struct {
				MessagesAdded []struct {
					Message struct {
						ID       string   `json:"id"`
						LabelIDs []string `json:"labelIds"`
					} `json:"message"`
				} `json:"messagesAdded"`
				LabelsAdded []struct {
					Message struct {
						ID string `json:"id"`
					} `json:"message"`
					LabelIDs []string `json:"labelIds"`
				} `json:"labelsAdded"`
			} `json:"history"`
			HistoryID     string `json:"historyId"`
			NextPageToken string `json:"nextPageToken"`
		}
		if err := a.get(ctx, "/history", q, &res); err != nil {
			return nil, "", err
		}
		add := func(id string) {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		for _, h := range res.History {
			for _, m := range h.MessagesAdded {
				if contains(m.Message.LabelIDs, labelID) {
					add(m.Message.ID)
				}
			}
			for _, m := range h.LabelsAdded {
				if contains(m.LabelIDs, labelID) {
					add(m.Message.ID)
				}
			}
		}
		if res.HistoryID != "" {
			last = res.HistoryID
		}
		if res.NextPageToken == "" {
			break
		}
		q.Set("pageToken", res.NextPageToken)
	}
	return ids, last, nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Changes implements provider.Provider: a list from since on the first read, then the
// history from the stored history id. A stale history id (404) lists from since
// again; the sync loop drops what it already has. Every new id is returned, so limit
// is not applied: a history read can't stop partway and resume.
func (a *Adapter) Changes(ctx context.Context, f provider.Folder, cur provider.Cursor, since time.Time, _ int) ([]provider.Ref, provider.Cursor, error) {
	var ids []string
	next := cur
	if cur.Token != "" {
		hids, last, err := a.history(ctx, f.Ref, cur.Token)
		switch {
		case err == nil:
			ids, next.Token = hids, last
		case restapi.StatusOf(err) == http.StatusNotFound:
			cur.Token = ""
		default:
			return nil, cur, err
		}
	}
	if cur.Token == "" {
		hid, err := a.historyID(ctx)
		if err != nil {
			return nil, cur, err
		}
		if ids, err = a.list(ctx, f.Ref, since); err != nil {
			return nil, cur, err
		}
		next.Token = hid
	}
	refs := make([]provider.Ref, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, provider.Ref{Folder: f.Ref, ID: id})
	}
	return refs, next, nil
}

// Fetch implements provider.Provider with users.messages.get?format=raw.
func (a *Adapter) Fetch(ctx context.Context, r provider.Ref) ([]byte, error) {
	var res struct {
		Raw string `json:"raw"`
	}
	if err := a.get(ctx, "/messages/"+url.PathEscape(r.ID), url.Values{"format": {"raw"}}, &res); err != nil {
		return nil, err
	}
	raw, err := base64.URLEncoding.DecodeString(res.Raw)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(res.Raw, "="))
	}
	if err != nil {
		return nil, errors.New("gmail: the raw message is not base64url")
	}
	return raw, nil
}

func (a *Adapter) modify(ctx context.Context, r provider.Ref, add, remove []string) error {
	body := map[string][]string{}
	if len(add) > 0 {
		body["addLabelIds"] = add
	}
	if len(remove) > 0 {
		body["removeLabelIds"] = remove
	}
	return a.post(ctx, "/messages/"+url.PathEscape(r.ID)+"/modify", body, nil)
}

// AfterFile implements provider.Provider: label adds Togavsu/Filed; move adds it and
// removes INBOX; delete moves to Trash, where Gmail's own retention applies.
func (a *Adapter) AfterFile(ctx context.Context, r provider.Ref, act provider.AfterFileAction) error {
	if act.MarkRead {
		if err := a.MarkRead(ctx, r, true); err != nil {
			return err
		}
	}
	switch act.Kind {
	case "", "leave":
		return nil
	case "label", "move":
		id, err := a.labelID(ctx, provider.GmailFiledLabel)
		if err != nil {
			return err
		}
		var remove []string
		if act.Kind == "move" {
			remove = []string{"INBOX"}
		}
		return a.modify(ctx, r, []string{id}, remove)
	case "delete":
		return a.post(ctx, "/messages/"+url.PathEscape(r.ID)+"/trash", nil, nil)
	}
	return errors.New("gmail: unknown after-filing action " + act.Kind)
}

// Move implements provider.Provider: the folder's label is added and the message's
// current label (unless SENT) removed. Filed means Togavsu/Filed. The message id
// doesn't change.
func (a *Adapter) Move(ctx context.Context, r provider.Ref, folder string) (provider.Ref, error) {
	if folder == provider.FiledFolder {
		folder = provider.GmailFiledLabel
	}
	id, err := a.labelID(ctx, folder)
	if err != nil {
		return provider.Ref{}, err
	}
	var remove []string
	if r.Folder != "" && r.Folder != "SENT" && r.Folder != id {
		remove = []string{r.Folder}
	}
	if err := a.modify(ctx, r, []string{id}, remove); err != nil {
		return provider.Ref{}, err
	}
	return provider.Ref{Folder: id, ID: r.ID}, nil
}

// MarkRead implements provider.Provider through the UNREAD label.
func (a *Adapter) MarkRead(ctx context.Context, r provider.Ref, read bool) error {
	if read {
		return a.modify(ctx, r, nil, []string{"UNREAD"})
	}
	return a.modify(ctx, r, []string{"UNREAD"}, nil)
}

// Send implements provider.Provider with users.messages.send. Gmail takes the
// recipients from the headers, so blind copies go in a Bcc header it strips.
func (a *Adapter) Send(ctx context.Context, raw []byte, env provider.Envelope) (string, error) {
	var res struct {
		ID string `json:"id"`
	}
	body := map[string]string{"raw": base64.URLEncoding.EncodeToString(restapi.WithBcc(raw, env))}
	if err := a.post(ctx, "/messages/send", body, &res); err != nil {
		return "", err
	}
	return res.ID, nil
}
