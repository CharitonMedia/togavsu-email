// Package graph is the adapter for Microsoft 365 and Outlook.com mailboxes over
// Microsoft Graph. A ref is the folder id and the message's immutable id, which
// survives a move.
package graph

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	gomail "github.com/emersion/go-message/mail"

	"github.com/CharitonMedia/togavsu-email/internal/oauth"
	"github.com/CharitonMedia/togavsu-email/internal/provider"
	"github.com/CharitonMedia/togavsu-email/internal/provider/restapi"
)

// LargeAttachments is the attachment total above which a send goes as a draft with
// upload sessions: sendMail takes at most 4 MB of base64 MIME.
const LargeAttachments = 3 << 20

// UploadChunk is one upload-session PUT: a multiple of 320 KiB, as Graph requires.
const UploadChunk = 10 * 320 << 10

// Adapter is one Graph mailbox.
type Adapter struct {
	API  *restapi.Client
	root string // https://graph.microsoft.com/v1.0
	me   string // root + /me

	mu      sync.Mutex
	folders map[string]string // lower display name -> id, top level
}

// New returns the adapter for c. c.APIBase and c.TokenURL override Microsoft's
// endpoints (tests point them at httptest servers on 127.0.0.1).
func New(c provider.Config, hc *http.Client) *Adapter {
	tokenURL := c.TokenURL
	if tokenURL == "" {
		tokenURL = oauth.For("microsoft", c.Tenant).TokenURL
	}
	base := c.APIBase
	if base == "" {
		base = oauth.GraphAPI
	}
	root := strings.TrimRight(base, "/") + "/v1.0"
	api := restapi.New(hc, c, tokenURL)
	api.Header = http.Header{"Prefer": {`IdType="ImmutableId"`}}
	return &Adapter{API: api, root: root, me: root + "/me"}
}

func (a *Adapter) call(ctx context.Context, method, u string, body, out any) ([]byte, error) {
	return a.API.Do(ctx, restapi.Request{Method: method, URL: u, JSON: body}, out)
}

// follow checks a next or delta link stays on the API's own host before the bearer
// token goes to it.
func (a *Adapter) follow(link string) (string, error) {
	if !strings.HasPrefix(link, a.root+"/") {
		return "", fmt.Errorf("graph: refusing a link off the API host: %.80s", link)
	}
	return link, nil
}

type folder struct {
	ID               string `json:"id"`
	DisplayName      string `json:"displayName"`
	ChildFolderCount int    `json:"childFolderCount"`
}

func (a *Adapter) folderPage(ctx context.Context, u string) ([]folder, error) {
	var all []folder
	for u != "" {
		var res struct {
			Value []folder `json:"value"`
			Next  string   `json:"@odata.nextLink"`
		}
		if _, err := a.call(ctx, http.MethodGet, u, nil, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Value...)
		u = ""
		if res.Next != "" {
			var err error
			if u, err = a.follow(res.Next); err != nil {
				return nil, err
			}
		}
	}
	return all, nil
}

// Check implements provider.Provider.
func (a *Adapter) Check(ctx context.Context) error {
	_, err := a.call(ctx, http.MethodGet, a.me+"/mailFolders/inbox?$select=id", nil, nil)
	return err
}

// Folders implements provider.Provider: every folder and subfolder, named by path.
func (a *Adapter) Folders(ctx context.Context) ([]provider.Folder, error) {
	top, err := a.folderPage(ctx, a.me+"/mailFolders?$top=100")
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.folders = map[string]string{}
	for _, f := range top {
		a.folders[strings.ToLower(f.DisplayName)] = f.ID
	}
	a.mu.Unlock()
	var out []provider.Folder
	var walk func(fs []folder, prefix string) error
	walk = func(fs []folder, prefix string) error {
		for _, f := range fs {
			name := prefix + f.DisplayName
			out = append(out, provider.Folder{Ref: f.ID, Name: name, Kind: provider.KindFor(name, "")})
			if f.ChildFolderCount > 0 {
				kids, err := a.folderPage(ctx, a.me+"/mailFolders/"+url.PathEscape(f.ID)+"/childFolders?$top=100")
				if err != nil {
					return err
				}
				if err := walk(kids, name+"/"); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(top, ""); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Changes implements provider.Provider with the folder's message delta, storing the
// delta link. A delta link Graph no longer knows starts again from since. Every new
// id is returned; a delta round can't stop partway, so limit is not applied.
func (a *Adapter) Changes(ctx context.Context, f provider.Folder, cur provider.Cursor, since time.Time, _ int) ([]provider.Ref, provider.Cursor, error) {
	start := a.me + "/mailFolders/" + url.PathEscape(f.Ref) + "/messages/delta?$select=id"
	if !since.IsZero() {
		start += "&$filter=" + url.QueryEscape("receivedDateTime ge "+since.UTC().Format(time.RFC3339))
	}
	u := start
	if cur.Token != "" {
		var err error
		if u, err = a.follow(cur.Token); err != nil {
			u = start
		}
	}
	var refs []provider.Ref
	seen := map[string]bool{}
	restarted := u == start
	for {
		var res struct {
			Value []struct {
				ID      string         `json:"id"`
				Removed map[string]any `json:"@removed"`
			} `json:"value"`
			Next  string `json:"@odata.nextLink"`
			Delta string `json:"@odata.deltaLink"`
		}
		_, err := a.call(ctx, http.MethodGet, u, nil, &res)
		if st := restapi.StatusOf(err); err != nil && !restarted && (st == http.StatusGone || st == http.StatusBadRequest || st == http.StatusNotFound) {
			u, restarted, refs = start, true, nil
			continue
		}
		if err != nil {
			return nil, cur, err
		}
		for _, m := range res.Value {
			if m.Removed == nil && m.ID != "" && !seen[m.ID] {
				seen[m.ID] = true
				refs = append(refs, provider.Ref{Folder: f.Ref, ID: m.ID})
			}
		}
		if res.Delta != "" {
			return refs, provider.Cursor{Token: res.Delta}, nil
		}
		if res.Next == "" {
			return refs, cur, nil
		}
		if u, err = a.follow(res.Next); err != nil {
			return nil, cur, err
		}
	}
}

func (a *Adapter) msg(r provider.Ref) string { return a.me + "/messages/" + url.PathEscape(r.ID) }

// Fetch implements provider.Provider with messages/{id}/$value.
func (a *Adapter) Fetch(ctx context.Context, r provider.Ref) ([]byte, error) {
	return a.call(ctx, http.MethodGet, a.msg(r)+"/$value", nil, nil)
}

// folderID finds a top-level folder by name, creating it when missing.
func (a *Adapter) folderID(ctx context.Context, name string) (string, error) {
	a.mu.Lock()
	id, ok := a.folders[strings.ToLower(name)]
	loaded := a.folders != nil
	a.mu.Unlock()
	if ok {
		return id, nil
	}
	if !loaded {
		if _, err := a.Folders(ctx); err != nil {
			return "", err
		}
		return a.folderID(ctx, name)
	}
	var f folder
	_, err := a.call(ctx, http.MethodPost, a.me+"/mailFolders", map[string]string{"displayName": name}, &f)
	if restapi.StatusOf(err) == http.StatusConflict {
		a.mu.Lock()
		a.folders = nil
		a.mu.Unlock()
		return a.folderID(ctx, name)
	}
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.folders[strings.ToLower(name)] = f.ID
	a.mu.Unlock()
	return f.ID, nil
}

func (a *Adapter) moveTo(ctx context.Context, r provider.Ref, dest string) (provider.Ref, error) {
	var m struct {
		ID             string `json:"id"`
		ParentFolderID string `json:"parentFolderId"`
	}
	if _, err := a.call(ctx, http.MethodPost, a.msg(r)+"/move", map[string]string{"destinationId": dest}, &m); err != nil {
		return provider.Ref{}, err
	}
	if m.ParentFolderID != "" {
		dest = m.ParentFolderID
	}
	if m.ID == "" {
		m.ID = r.ID
	}
	return provider.Ref{Folder: dest, ID: m.ID}, nil
}

// Move implements provider.Provider: messages/{id}/move to the folder, created when
// missing.
func (a *Adapter) Move(ctx context.Context, r provider.Ref, folder string) (provider.Ref, error) {
	id, err := a.folderID(ctx, folder)
	if err != nil {
		return provider.Ref{}, err
	}
	return a.moveTo(ctx, r, id)
}

// AfterFile implements provider.Provider: label adds the Togavsu/Filed category, move
// goes to Filed, delete moves to Deleted Items.
func (a *Adapter) AfterFile(ctx context.Context, r provider.Ref, act provider.AfterFileAction) error {
	if act.MarkRead {
		if err := a.MarkRead(ctx, r, true); err != nil {
			return err
		}
	}
	switch act.Kind {
	case "", "leave":
		return nil
	case "label":
		var m struct {
			Categories []string `json:"categories"`
		}
		if _, err := a.call(ctx, http.MethodGet, a.msg(r)+"?$select=categories", nil, &m); err != nil {
			return err
		}
		for _, c := range m.Categories {
			if c == provider.GmailFiledLabel {
				return nil
			}
		}
		_, err := a.call(ctx, http.MethodPatch, a.msg(r), map[string]any{"categories": append(m.Categories, provider.GmailFiledLabel)}, nil)
		return err
	case "move":
		_, err := a.Move(ctx, r, provider.FiledFolder)
		return err
	case "delete":
		_, err := a.moveTo(ctx, r, "deleteditems")
		return err
	}
	return errors.New("graph: unknown after-filing action " + act.Kind)
}

// MarkRead implements provider.Provider.
func (a *Adapter) MarkRead(ctx context.Context, r provider.Ref, read bool) error {
	_, err := a.call(ctx, http.MethodPatch, a.msg(r), map[string]bool{"isRead": read}, nil)
	return err
}

func (a *Adapter) mime(ctx context.Context, u string, raw []byte, out any) error {
	_, err := a.API.Do(ctx, restapi.Request{Method: http.MethodPost, URL: u, ContentType: "text/plain",
		Body: []byte(base64.StdEncoding.EncodeToString(raw))}, out)
	return err
}

type attachment struct {
	name, ctype string
	data        []byte
}

// split takes the attachments out of raw and returns the message without them.
func split(raw []byte) ([]byte, []attachment, error) {
	mr, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, err
	}
	var atts []attachment
	var text []byte
	ctype := "text/plain; charset=utf-8"
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		b, err := io.ReadAll(p.Body)
		if err != nil {
			return nil, nil, err
		}
		switch h := p.Header.(type) {
		case *gomail.AttachmentHeader:
			name, _ := h.Filename()
			ct, _, _ := h.ContentType()
			atts = append(atts, attachment{name: name, ctype: ct, data: b})
		case *gomail.InlineHeader:
			if text == nil {
				text = b
				if ct := h.Get("Content-Type"); ct != "" {
					ctype = ct
				}
			}
		}
	}
	h := mr.Header
	h.Del("Content-Transfer-Encoding")
	h.Set("Content-Type", ctype)
	var buf bytes.Buffer
	w, err := gomail.CreateSingleInlineWriter(&buf, h)
	if err != nil {
		return nil, nil, err
	}
	if _, err := w.Write(text); err != nil {
		return nil, nil, err
	}
	if err := w.Close(); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), atts, nil
}

// Send implements provider.Provider. A message whose attachments total 3 MB or less
// goes through sendMail as MIME; a larger one is created as a draft from its MIME
// without the attachments, gets each attachment through an upload session, and is
// then sent. Blind copies go in a Bcc header, which Graph strips. The answer is the
// message's Message-ID.
func (a *Adapter) Send(ctx context.Context, raw []byte, env provider.Envelope) (string, error) {
	raw = restapi.WithBcc(raw, env)
	id := restapi.MessageID(raw)
	body, atts, err := split(raw)
	total := 0
	for _, at := range atts {
		total += len(at.data)
	}
	if err != nil || total <= LargeAttachments {
		return id, a.mime(ctx, a.me+"/sendMail", raw, nil)
	}
	var draft struct {
		ID string `json:"id"`
	}
	if err := a.mime(ctx, a.me+"/messages", body, &draft); err != nil {
		return "", err
	}
	if draft.ID == "" {
		return "", errors.New("graph: the draft has no id")
	}
	d := provider.Ref{ID: draft.ID}
	for _, at := range atts {
		if err := a.upload(ctx, d, at); err != nil {
			return "", err
		}
	}
	if _, err := a.call(ctx, http.MethodPost, a.msg(d)+"/send", nil, nil); err != nil {
		return "", err
	}
	return id, nil
}

func (a *Adapter) upload(ctx context.Context, d provider.Ref, at attachment) error {
	if at.ctype == "" {
		at.ctype = "application/octet-stream"
	}
	var s struct {
		UploadURL string `json:"uploadUrl"`
	}
	item := map[string]any{"AttachmentItem": map[string]any{"attachmentType": "file", "name": at.name, "size": len(at.data), "contentType": at.ctype}}
	if _, err := a.call(ctx, http.MethodPost, a.msg(d)+"/attachments/createUploadSession", item, &s); err != nil {
		return err
	}
	up, err := url.Parse(s.UploadURL)
	root, _ := url.Parse(a.root)
	if err != nil || (up.Scheme != "https" && up.Host != root.Host) {
		return errors.New("graph: the upload session has no https upload URL")
	}
	for off := 0; off < len(at.data); off += UploadChunk {
		end := off + UploadChunk
		if end > len(at.data) {
			end = len(at.data)
		}
		h := http.Header{"Content-Range": {fmt.Sprintf("bytes %d-%d/%d", off, end-1, len(at.data))}}
		// The upload URL is pre-authorized; the bearer token never goes to it.
		if _, err := a.API.Do(ctx, restapi.Request{Method: http.MethodPut, URL: s.UploadURL, ContentType: "application/octet-stream",
			Body: at.data[off:end], NoAuth: true, Header: h}, nil); err != nil {
			return err
		}
	}
	return nil
}
