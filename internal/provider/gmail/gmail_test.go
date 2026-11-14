package gmail

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/provider"
	"github.com/CharitonMedia/togavsu-email/internal/testmail"
)

func fakeToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type msg struct {
	raw    []byte
	labels map[string]bool
}

// fakeGmail speaks the parts of the Gmail API the adapter uses.
type fakeGmail struct {
	mu        sync.Mutex
	access    string
	refresh   string
	refreshes int
	history   string
	stale     bool
	labels    []label
	msgs      map[string]*msg
	order     []string
	added     []string // ids added since the history id
	sent      []string
	trashed   []string
	throttle  int
	listQ     []string
}

func newFake() *fakeGmail {
	return &fakeGmail{access: fakeToken(), refresh: fakeToken(), history: "100", msgs: map[string]*msg{},
		labels: []label{{ID: "INBOX", Name: "INBOX", Type: "system"}, {ID: "SENT", Name: "SENT", Type: "system"},
			{ID: "TRASH", Name: "TRASH", Type: "system"}, {ID: "SPAM", Name: "SPAM", Type: "system"},
			{ID: "Label_1", Name: "Clients", Type: "user"}}}
}

func (g *fakeGmail) add(id string, raw []byte, labels ...string) {
	m := &msg{raw: raw, labels: map[string]bool{}}
	for _, l := range labels {
		m.labels[l] = true
	}
	g.msgs[id] = m
	g.order = append(g.order, id)
	g.added = append(g.added, id)
}

func (g *fakeGmail) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.URL.Path == "/token" {
		r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != g.refresh {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		g.refreshes++
		g.access = fakeToken()
		json.NewEncoder(w).Encode(map[string]any{"access_token": g.access, "expires_in": 3600})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+g.access {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if g.throttle > 0 {
		g.throttle--
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/gmail/v1/users/me")
	out := func(v any) { json.NewEncoder(w).Encode(v) }
	switch {
	case p == "/profile":
		out(map[string]string{"emailAddress": "clerk@firm.test", "historyId": g.history})
	case p == "/labels" && r.Method == http.MethodGet:
		out(map[string]any{"labels": g.labels})
	case p == "/labels" && r.Method == http.MethodPost:
		var l label
		json.NewDecoder(r.Body).Decode(&l)
		l.ID, l.Type = "Label_"+string(rune('0'+len(g.labels))), "user"
		g.labels = append(g.labels, l)
		out(l)
	case p == "/messages":
		g.listQ = append(g.listQ, r.URL.Query().Get("q"))
		lab := r.URL.Query().Get("labelIds")
		var ms []map[string]string
		for i := len(g.order) - 1; i >= 0; i-- {
			if m := g.msgs[g.order[i]]; m != nil && m.labels[lab] {
				ms = append(ms, map[string]string{"id": g.order[i]})
			}
		}
		// Two pages, to follow nextPageToken.
		if r.URL.Query().Get("pageToken") == "" && len(ms) > 1 {
			out(map[string]any{"messages": ms[:1], "nextPageToken": "p2"})
			return
		}
		if r.URL.Query().Get("pageToken") == "p2" {
			ms = ms[1:]
		}
		out(map[string]any{"messages": ms})
	case p == "/history":
		if g.stale || r.URL.Query().Get("startHistoryId") != g.history {
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
			return
		}
		lab := r.URL.Query().Get("labelId")
		var h []any
		for _, id := range g.added {
			var ls []string
			for l := range g.msgs[id].labels {
				ls = append(ls, l)
			}
			if g.msgs[id].labels[lab] {
				h = append(h, map[string]any{"messagesAdded": []any{map[string]any{"message": map[string]any{"id": id, "labelIds": ls}}}})
			}
		}
		g.added = nil
		g.history = "200"
		out(map[string]any{"history": h, "historyId": g.history})
	case strings.HasPrefix(p, "/messages/send"):
		var b struct{ Raw string }
		json.NewDecoder(r.Body).Decode(&b)
		raw, _ := base64.URLEncoding.DecodeString(b.Raw)
		g.sent = append(g.sent, string(raw))
		out(map[string]string{"id": "sent1"})
	case strings.HasSuffix(p, "/modify"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/messages/"), "/modify")
		m := g.msgs[id]
		if m == nil {
			http.NotFound(w, r)
			return
		}
		var b struct{ AddLabelIds, RemoveLabelIds []string }
		json.NewDecoder(r.Body).Decode(&b)
		for _, l := range b.AddLabelIds {
			m.labels[l] = true
		}
		for _, l := range b.RemoveLabelIds {
			delete(m.labels, l)
		}
		out(map[string]string{"id": id})
	case strings.HasSuffix(p, "/trash"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/messages/"), "/trash")
		g.trashed = append(g.trashed, id)
		g.msgs[id].labels = map[string]bool{"TRASH": true}
		out(map[string]string{"id": id})
	case strings.HasPrefix(p, "/messages/"):
		id := strings.TrimPrefix(p, "/messages/")
		m := g.msgs[id]
		if m == nil || r.URL.Query().Get("format") != "raw" {
			http.Error(w, `{"error":{"code":404}}`, http.StatusNotFound)
			return
		}
		out(map[string]string{"id": id, "raw": base64.URLEncoding.EncodeToString(m.raw)})
	default:
		http.NotFound(w, r)
	}
}

type rig struct {
	g      *fakeGmail
	a      *Adapter
	saved  []provider.Token
	sleeps []time.Duration
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{g: newFake()}
	srv := httptest.NewServer(r.g)
	t.Cleanup(srv.Close)
	testmail.MustLoopback(strings.Split(strings.TrimPrefix(srv.URL, "http://"), ":")[0])
	c := provider.Config{AccountID: "g1", Provider: "gmail", Address: "clerk@firm.test", APIBase: srv.URL, TokenURL: srv.URL + "/token",
		ClientID: "client", ClientSecret: fakeToken(),
		Token:     &provider.Token{AccessToken: "expired", RefreshToken: r.g.refresh, Expiry: time.Now().Add(time.Hour)},
		SaveToken: func(_ context.Context, t provider.Token) error { r.saved = append(r.saved, t); return nil }}
	r.a = New(c, srv.Client())
	r.a.API.Sleep = func(_ context.Context, d time.Duration) error { r.sleeps = append(r.sleeps, d); return nil }
	return r
}

func raw(subject string) []byte {
	return testmail.Msg{ID: subject + "@client.test", From: "pat@client.test", To: []string{"clerk@firm.test"}, Subject: subject,
		Date: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC), Text: "Body " + subject}.Raw()
}

func TestRefreshOn401AndFolders(t *testing.T) {
	r := newRig(t)
	fs, err := r.a.Folders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.g.refreshes != 1 || len(r.saved) != 1 || r.saved[0].RefreshToken != r.g.refresh {
		t.Fatalf("refresh %d saved %+v", r.g.refreshes, r.saved)
	}
	kinds := map[string]string{}
	for _, f := range fs {
		kinds[f.Name] = f.Kind
	}
	if kinds["INBOX"] != provider.KindInbox || kinds["SENT"] != provider.KindSent || kinds["Clients"] != provider.KindOther || len(fs) != 3 {
		t.Fatalf("folders %+v", fs)
	}
}

func TestRevokedRefreshIsErrAuth(t *testing.T) {
	r := newRig(t)
	r.g.refresh = "revoked"
	if err := r.a.Check(context.Background()); !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
}

func TestThrottledHonorsRetryAfter(t *testing.T) {
	r := newRig(t)
	r.g.throttle = 2
	if err := r.a.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.sleeps) != 2 || r.sleeps[0] != 7*time.Second {
		t.Fatalf("sleeps %v", r.sleeps)
	}
	r.g.throttle = 10
	if err := r.a.Check(context.Background()); !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("want ErrUnavailable after repeated throttling, got %v", err)
	}
}

func TestChangesHistoryAndStaleFallback(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.g.add("m1", raw("one"), "INBOX")
	r.g.add("m2", raw("two"), "INBOX")
	r.g.add("m3", raw("other"), "Label_1")
	inbox := provider.Folder{Ref: "INBOX"}
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	refs, cur, err := r.a.Changes(ctx, inbox, provider.Cursor{}, since, 200)
	if err != nil || len(refs) != 2 || refs[0].ID != "m1" || cur.Token != "100" {
		t.Fatalf("first read %v %+v %v", refs, cur, err)
	}
	if r.g.listQ[0] != "after:1767225600" {
		t.Fatalf("list q %q", r.g.listQ)
	}
	r.g.added = nil
	r.g.add("m4", raw("four"), "INBOX", "UNREAD")
	refs, cur, err = r.a.Changes(ctx, inbox, cur, since, 200)
	if err != nil || len(refs) != 1 || refs[0].ID != "m4" || cur.Token != "200" {
		t.Fatalf("history %v %+v %v", refs, cur, err)
	}
	// A stale history id lists from since again.
	r.g.stale = true
	refs, cur, err = r.a.Changes(ctx, inbox, cur, since, 200)
	if err != nil || len(refs) != 3 || cur.Token != "200" {
		t.Fatalf("stale fallback %v %+v %v", refs, cur, err)
	}
	b, err := r.a.Fetch(ctx, refs[0])
	if err != nil || !strings.Contains(string(b), "Body one") {
		t.Fatalf("fetch %q %v", b, err)
	}
	if _, err := r.a.Fetch(ctx, provider.Ref{Folder: "INBOX", ID: "gone"}); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestAfterFileMoveAndSend(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.g.add("m1", raw("label"), "INBOX", "UNREAD")
	r.g.add("m2", raw("court"), "INBOX", "UNREAD")
	r.g.add("m3", raw("delete"), "INBOX")
	r.g.add("m4", raw("later"), "INBOX")
	in := func(id string) provider.Ref { return provider.Ref{Folder: "INBOX", ID: id} }
	if err := r.a.AfterFile(ctx, in("m1"), provider.AfterFileAction{Kind: "label"}); err != nil {
		t.Fatal(err)
	}
	filed, _ := r.a.labelID(ctx, provider.GmailFiledLabel)
	if m := r.g.msgs["m1"].labels; !m["INBOX"] || !m[filed] || !m["UNREAD"] {
		t.Fatalf("label: %v", m)
	}
	if err := r.a.AfterFile(ctx, in("m2"), provider.AfterFileAction{Kind: "move", MarkRead: true}); err != nil {
		t.Fatal(err)
	}
	if m := r.g.msgs["m2"].labels; m["INBOX"] || !m[filed] || m["UNREAD"] {
		t.Fatalf("move: %v", m)
	}
	if err := r.a.AfterFile(ctx, in("m3"), provider.AfterFileAction{Kind: "delete"}); err != nil || len(r.g.trashed) != 1 {
		t.Fatalf("delete %v %v", r.g.trashed, err)
	}
	nr, err := r.a.Move(ctx, in("m4"), provider.ReviewLaterFolder)
	if err != nil || nr.ID != "m4" || nr.Folder == "INBOX" {
		t.Fatalf("move %+v %v", nr, err)
	}
	if m := r.g.msgs["m4"].labels; m["INBOX"] || !m[nr.Folder] {
		t.Fatalf("review later %v", m)
	}
	out := testmail.Msg{ID: "out@firm.test", From: "clerk@firm.test", To: []string{"pat@client.test"}, Subject: "Re: x", Text: "hi"}.Raw()
	id, err := r.a.Send(ctx, out, provider.Envelope{From: "clerk@firm.test", To: []string{"pat@client.test", "secret@client.test"}})
	if err != nil || id != "sent1" || len(r.g.sent) != 1 || !strings.HasPrefix(r.g.sent[0], "Bcc: secret@client.test\r\n") {
		t.Fatalf("send %q %v %q", id, err, r.g.sent)
	}
}
