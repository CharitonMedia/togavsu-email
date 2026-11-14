package graph

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/provider"
	"github.com/CharitonMedia/togavsu-email/internal/send"
	"github.com/CharitonMedia/togavsu-email/internal/testmail"
)

func fakeToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type gmsg struct {
	folder     string
	raw        []byte
	read       bool
	categories []string
}

// fakeGraph speaks the parts of Microsoft Graph the adapter uses.
type fakeGraph struct {
	mu        sync.Mutex
	url       string
	access    string
	refresh   string
	refreshes int
	folders   []folder
	children  map[string][]folder
	msgs      map[string]*gmsg
	order     []string
	round     int
	staleLink bool
	throttle  int
	sendMail  [][]byte
	drafts    map[string][]byte
	uploads   map[string][]byte
	ranges    []string
	sent      []string
	authOnPut bool
	seq       int
}

func newFake() *fakeGraph {
	return &fakeGraph{access: fakeToken(), refresh: fakeToken(), msgs: map[string]*gmsg{}, drafts: map[string][]byte{}, uploads: map[string][]byte{},
		folders: []folder{{ID: "inbox-id", DisplayName: "Inbox", ChildFolderCount: 1}, {ID: "sent-id", DisplayName: "Sent Items"},
			{ID: "del-id", DisplayName: "Deleted Items"}, {ID: "junk-id", DisplayName: "Junk Email"}},
		children: map[string][]folder{"inbox-id": {{ID: "clients-id", DisplayName: "Clients"}}}}
}

func (g *fakeGraph) add(id, folder string, raw []byte) {
	g.msgs[id] = &gmsg{folder: folder, raw: raw}
	g.order = append(g.order, id)
}

func (g *fakeGraph) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := func(v any) { json.NewEncoder(w).Encode(v) }
	if r.URL.Path == "/token" {
		r.ParseForm()
		if r.Form.Get("refresh_token") != g.refresh {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		g.refreshes++
		g.access = fakeToken()
		out(map[string]any{"access_token": g.access, "expires_in": 3600})
		return
	}
	if strings.HasPrefix(r.URL.Path, "/upload/") {
		if r.Header.Get("Authorization") != "" {
			g.authOnPut = true
		}
		b, _ := io.ReadAll(r.Body)
		k := strings.TrimPrefix(r.URL.Path, "/upload/")
		g.uploads[k] = append(g.uploads[k], b...)
		g.ranges = append(g.ranges, r.Header.Get("Content-Range"))
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+g.access {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Header.Get("Prefer") != `IdType="ImmutableId"` {
		http.Error(w, "no immutable ids", http.StatusBadRequest)
		return
	}
	if g.throttle > 0 {
		g.throttle--
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/v1.0/me")
	switch {
	case p == "/mailFolders/inbox":
		out(map[string]string{"id": "inbox-id"})
	case p == "/mailFolders" && r.Method == http.MethodGet:
		if r.URL.Query().Get("$skip") == "" {
			out(map[string]any{"value": g.folders[:2], "@odata.nextLink": g.url + "/v1.0/me/mailFolders?$skip=2"})
			return
		}
		out(map[string]any{"value": g.folders[2:]})
	case p == "/mailFolders" && r.Method == http.MethodPost:
		var f folder
		json.NewDecoder(r.Body).Decode(&f)
		f.ID = strings.ToLower(f.DisplayName) + "-id"
		g.folders = append(g.folders, f)
		out(f)
	case strings.HasSuffix(p, "/childFolders"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/mailFolders/"), "/childFolders")
		out(map[string]any{"value": g.children[id]})
	case strings.HasSuffix(p, "/messages/delta"):
		fid := strings.TrimSuffix(strings.TrimPrefix(p, "/mailFolders/"), "/messages/delta")
		tok := r.URL.Query().Get("$deltatoken")
		if tok != "" && (g.staleLink || tok != strconv.Itoa(g.round)) {
			http.Error(w, `{"error":{"code":"SyncStateNotFound"}}`, http.StatusGone)
			return
		}
		from := 0
		if tok != "" {
			from, _ = strconv.Atoi(tok)
		}
		var v []map[string]any
		for _, id := range g.order[from:] {
			if g.msgs[id].folder == fid {
				v = append(v, map[string]any{"id": id})
			}
		}
		v = append(v, map[string]any{"id": "gone-id", "@removed": map[string]string{"reason": "deleted"}})
		g.round = len(g.order)
		out(map[string]any{"value": v, "@odata.deltaLink": fmt.Sprintf("%s/v1.0/me/mailFolders/%s/messages/delta?$deltatoken=%d", g.url, fid, g.round)})
	case p == "/sendMail":
		b, _ := io.ReadAll(r.Body)
		raw, _ := base64.StdEncoding.DecodeString(string(b))
		g.sendMail = append(g.sendMail, raw)
		w.WriteHeader(http.StatusAccepted)
	case p == "/messages" && r.Method == http.MethodPost:
		b, _ := io.ReadAll(r.Body)
		raw, _ := base64.StdEncoding.DecodeString(string(b))
		g.seq++
		id := fmt.Sprintf("draft-%d", g.seq)
		g.drafts[id] = raw
		w.WriteHeader(http.StatusCreated)
		out(map[string]string{"id": id})
	case strings.HasSuffix(p, "/attachments/createUploadSession"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/messages/"), "/attachments/createUploadSession")
		var b struct{ AttachmentItem map[string]any }
		json.NewDecoder(r.Body).Decode(&b)
		out(map[string]string{"uploadUrl": g.url + "/upload/" + id + "-" + b.AttachmentItem["name"].(string)})
	case strings.HasSuffix(p, "/send"):
		g.sent = append(g.sent, strings.TrimSuffix(strings.TrimPrefix(p, "/messages/"), "/send"))
		w.WriteHeader(http.StatusAccepted)
	case strings.HasSuffix(p, "/move"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/messages/"), "/move")
		var b struct{ DestinationID string }
		json.NewDecoder(r.Body).Decode(&b)
		if b.DestinationID == "deleteditems" {
			b.DestinationID = "del-id"
		}
		g.msgs[id].folder = b.DestinationID
		out(map[string]string{"id": id, "parentFolderId": b.DestinationID})
	case strings.HasSuffix(p, "/$value"):
		m := g.msgs[strings.TrimSuffix(strings.TrimPrefix(p, "/messages/"), "/$value")]
		if m == nil {
			http.Error(w, `{"error":{"code":"ErrorItemNotFound"}}`, http.StatusNotFound)
			return
		}
		w.Write(m.raw)
	case strings.HasPrefix(p, "/messages/"):
		m := g.msgs[strings.TrimPrefix(p, "/messages/")]
		if m == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPatch {
			var b struct {
				IsRead     *bool
				Categories []string
			}
			json.NewDecoder(r.Body).Decode(&b)
			if b.IsRead != nil {
				m.read = *b.IsRead
			}
			if b.Categories != nil {
				m.categories = b.Categories
			}
		}
		out(map[string]any{"categories": m.categories})
	default:
		http.NotFound(w, r)
	}
}

type rig struct {
	g      *fakeGraph
	a      *Adapter
	sleeps []time.Duration
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{g: newFake()}
	srv := httptest.NewServer(r.g)
	t.Cleanup(srv.Close)
	r.g.url = srv.URL
	testmail.MustLoopback(strings.Split(strings.TrimPrefix(srv.URL, "http://"), ":")[0])
	c := provider.Config{AccountID: "m1", Provider: "microsoft", Address: "clerk@firm.test", APIBase: srv.URL, TokenURL: srv.URL + "/token",
		ClientID: "client", ClientSecret: fakeToken(),
		Token: &provider.Token{AccessToken: "old", RefreshToken: r.g.refresh, Expiry: time.Now().Add(time.Hour)}}
	r.a = New(c, srv.Client())
	r.a.API.Sleep = func(_ context.Context, d time.Duration) error { r.sleeps = append(r.sleeps, d); return nil }
	return r
}

func raw(s string) []byte {
	return testmail.Msg{ID: s + "@client.test", From: "pat@client.test", To: []string{"clerk@firm.test"}, Subject: s, Text: "Body " + s,
		Date: time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)}.Raw()
}

func TestFoldersRefreshAndThrottle(t *testing.T) {
	r := newRig(t)
	r.g.throttle = 1
	fs, err := r.a.Folders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.g.refreshes != 1 || len(r.sleeps) != 1 || r.sleeps[0] != 3*time.Second {
		t.Fatalf("refreshes %d sleeps %v", r.g.refreshes, r.sleeps)
	}
	kinds := map[string]string{}
	for _, f := range fs {
		kinds[f.Name] = f.Kind
	}
	want := map[string]string{"Inbox": provider.KindInbox, "Inbox/Clients": provider.KindOther, "Sent Items": provider.KindSent,
		"Deleted Items": provider.KindSkip, "Junk Email": provider.KindSkip}
	for k, v := range want {
		if kinds[k] != v {
			t.Errorf("%s: %q want %q (%v)", k, kinds[k], v, kinds)
		}
	}
	r.g.refresh = "revoked"
	r.g.access = "rotated"
	if err := r.a.Check(context.Background()); !errors.Is(err, provider.ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
}

func TestDeltaAndStaleLink(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.g.add("a", "inbox-id", raw("one"))
	r.g.add("b", "clients-id", raw("two"))
	in := provider.Folder{Ref: "inbox-id"}
	refs, cur, err := r.a.Changes(ctx, in, provider.Cursor{}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 200)
	if err != nil || len(refs) != 1 || refs[0].ID != "a" || !strings.Contains(cur.Token, "$deltatoken=2") {
		t.Fatalf("first %v %+v %v", refs, cur, err)
	}
	r.g.add("c", "inbox-id", raw("three"))
	refs, cur, err = r.a.Changes(ctx, in, cur, time.Time{}, 200)
	if err != nil || len(refs) != 1 || refs[0].ID != "c" {
		t.Fatalf("delta %v %v", refs, err)
	}
	r.g.staleLink = true
	refs, _, err = r.a.Changes(ctx, in, cur, time.Time{}, 200)
	if err != nil || len(refs) != 2 {
		t.Fatalf("stale %v %v", refs, err)
	}
	// A delta link off the API host is never followed with the token.
	if _, _, err := r.a.Changes(ctx, in, provider.Cursor{Token: "https://evil.test/x"}, time.Time{}, 0); err != nil {
		t.Fatalf("off-host link should restart from the folder: %v", err)
	}
	b, err := r.a.Fetch(ctx, refs[0])
	if err != nil || !strings.Contains(string(b), "Body one") {
		t.Fatalf("fetch %q %v", b, err)
	}
	if _, err := r.a.Fetch(ctx, provider.Ref{ID: "nope"}); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestAfterFileAndMove(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	for _, id := range []string{"l", "c", "d", "r"} {
		r.g.add(id, "inbox-id", raw(id))
	}
	ref := func(id string) provider.Ref { return provider.Ref{Folder: "inbox-id", ID: id} }
	if err := r.a.AfterFile(ctx, ref("l"), provider.AfterFileAction{Kind: "label"}); err != nil {
		t.Fatal(err)
	}
	if m := r.g.msgs["l"]; m.folder != "inbox-id" || len(m.categories) != 1 || m.categories[0] != provider.GmailFiledLabel {
		t.Fatalf("label %+v", m)
	}
	if err := r.a.AfterFile(ctx, ref("c"), provider.AfterFileAction{Kind: "move", MarkRead: true}); err != nil {
		t.Fatal(err)
	}
	if m := r.g.msgs["c"]; m.folder != "filed-id" || !m.read {
		t.Fatalf("court %+v", m)
	}
	if err := r.a.AfterFile(ctx, ref("d"), provider.AfterFileAction{Kind: "delete"}); err != nil || r.g.msgs["d"].folder != "del-id" {
		t.Fatalf("delete %v %+v", err, r.g.msgs["d"])
	}
	nr, err := r.a.Move(ctx, ref("r"), provider.ReviewLaterFolder)
	if err != nil || nr.Folder != "review-later-id" || nr.ID != "r" {
		t.Fatalf("move %+v %v", nr, err)
	}
	if err := r.a.MarkRead(ctx, nr, false); err != nil {
		t.Fatal(err)
	}
}

func TestSendSmallAndLarge(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	small, _ := send.Build(send.Message{From: "clerk@firm.test", To: []string{"pat@client.test"}, Subject: "Small", Text: "hi",
		MessageID: "small@firm.test", Date: time.Now()})
	id, err := r.a.Send(ctx, small, provider.Envelope{To: []string{"pat@client.test", "hidden@client.test"}})
	if err != nil || id != "<small@firm.test>" || len(r.g.sendMail) != 1 || !bytes.HasPrefix(r.g.sendMail[0], []byte("Bcc: hidden@client.test\r\n")) {
		t.Fatalf("small %q %v %d", id, err, len(r.g.sendMail))
	}
	big := bytes.Repeat([]byte("0123456789abcdef"), (4<<20)/16)
	large, _ := send.Build(send.Message{From: "clerk@firm.test", To: []string{"pat@client.test"}, Subject: "Large", Text: "see attached",
		MessageID: "large@firm.test", InReplyTo: "<orig@client.test>", Date: time.Now(),
		Attachments: []send.Attachment{{Name: "big.pdf", MediaType: "application/pdf", Data: big}}})
	if _, err := r.a.Send(ctx, large, provider.Envelope{To: []string{"pat@client.test"}}); err != nil {
		t.Fatal(err)
	}
	if len(r.g.sendMail) != 1 || len(r.g.drafts) != 1 || len(r.g.sent) != 1 {
		t.Fatalf("large went by sendMail %d drafts %d sent %d", len(r.g.sendMail), len(r.g.drafts), len(r.g.sent))
	}
	d := string(r.g.drafts[r.g.sent[0]])
	if !strings.Contains(d, "see attached") || !strings.Contains(d, "orig@client.test") || strings.Contains(d, "big.pdf") || len(d) > 10000 {
		t.Fatalf("draft MIME %.400q", d)
	}
	up := r.g.uploads[r.g.sent[0]+"-big.pdf"]
	if !bytes.Equal(up, big) || len(r.g.ranges) != 2 || r.g.authOnPut {
		t.Fatalf("upload %d bytes, ranges %v, token on upload %v", len(up), r.g.ranges, r.g.authOnPut)
	}
	if r.g.ranges[0] != fmt.Sprintf("bytes 0-%d/%d", UploadChunk-1, len(big)) {
		t.Fatalf("range %s", r.g.ranges[0])
	}
}
