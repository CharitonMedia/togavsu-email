// Package fake is an in-memory mailbox set with folders, UIDs, UIDVALIDITY, labels, read
// flags and a sent list. It can be told to fail sign-in, time out, return a stale
// cursor, change UIDVALIDITY, drop a message between list and fetch, and fail sends.
package fake

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/mailparse"
	"github.com/CharitonMedia/togavsu-email/internal/provider"
)

// Sent is one message the fake sent.
type Sent struct {
	Raw []byte
	Env provider.Envelope
	ID  string
}

type msg struct {
	uid    uint32
	raw    []byte
	seen   bool
	labels map[string]bool
	date   time.Time
}

type folder struct {
	name        string
	uidValidity uint32
	nextUID     uint32
	msgs        map[uint32]*msg
}

// Mailbox is one fake account.
type Mailbox struct {
	mu      sync.Mutex
	Address string
	folders map[string]*folder
	sent    []Sent

	// Failure knobs.
	FailSignIn  bool
	Timeout     bool
	StaleCursor bool
	FailSend    error
	dropOnFetch map[string]bool

	// Records.
	ReadChanges []string // "folder/uid=true|false" for every read-flag change
	Deleted     []string
	Moved       []string // "folder/uid->target"
	Labeled     []string
	Fetches     map[string]int
	Events      []string // ordered provider calls that change the mailbox
}

// New returns a mailbox with INBOX and Sent.
func New(address string) *Mailbox {
	m := &Mailbox{Address: address, folders: map[string]*folder{}, dropOnFetch: map[string]bool{}, Fetches: map[string]int{}}
	m.AddFolder("INBOX")
	m.AddFolder("Sent")
	return m
}

// AddFolder creates a folder.
func (m *Mailbox) AddFolder(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addFolderLocked(name)
}

func (m *Mailbox) addFolderLocked(name string) *folder {
	if f, ok := m.folders[name]; ok {
		return f
	}
	f := &folder{name: name, uidValidity: uint32(len(m.folders) + 1000), nextUID: one, msgs: map[uint32]*msg{}}
	m.folders[name] = f
	return f
}

// Deliver adds raw to folder and returns its ref.
func (m *Mailbox) Deliver(folderName string, raw []byte) provider.Ref {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.addFolderLocked(folderName)
	uid := f.nextUID
	f.nextUID++
	f.msgs[uid] = &msg{uid: uid, raw: raw, labels: map[string]bool{}, date: time.Now()}
	return ref(f, uid)
}

func ref(f *folder, uid uint32) provider.Ref {
	return provider.Ref{Folder: f.name, ID: strconv.FormatUint(uint64(f.uidValidity), decimal) + "." + strconv.FormatUint(uint64(uid), decimal)}
}

// ChangeUIDValidity renumbers a folder the way a server does after a rebuild: a new
// UIDVALIDITY and new UIDs for the same messages.
func (m *Mailbox) ChangeUIDValidity(folderName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.folders[folderName]
	f.uidValidity += uidValidityStep
	old := f.msgs
	f.msgs = map[uint32]*msg{}
	uids := make([]int, 0, len(old))
	for u := range old {
		uids = append(uids, int(u))
	}
	sort.Ints(uids)
	f.nextUID = one
	for _, u := range uids {
		mm := old[uint32(u)]
		mm.uid = f.nextUID
		f.msgs[f.nextUID] = mm
		f.nextUID++
	}
}

// DropOnFetch makes r vanish when it is fetched (removed between list and fetch).
func (m *Mailbox) DropOnFetch(r provider.Ref) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropOnFetch[r.String()] = true
}

// Remove deletes r from the mailbox without recording it as a module action.
func (m *Mailbox) Remove(r provider.Ref) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f, uid, ok := m.find(r); ok {
		delete(f.msgs, uid)
	}
}

// SentMessages returns what was sent.
func (m *Mailbox) SentMessages() []Sent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Sent(nil), m.sent...)
}

// Has reports whether r is still in its folder.
func (m *Mailbox) Has(r provider.Ref) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, _, ok := m.find(r)
	return ok
}

// FolderCount is how many messages folder holds.
func (m *Mailbox) FolderCount(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f, ok := m.folders[name]; ok {
		return len(f.msgs)
	}
	return zero
}

// Seen reports a message's read flag.
func (m *Mailbox) Seen(r provider.Ref) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f, uid, ok := m.find(r); ok {
		return f.msgs[uid].seen
	}
	return false
}

// Labels returns a message's labels.
func (m *Mailbox) Labels(r provider.Ref) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	if f, uid, ok := m.find(r); ok {
		for l := range f.msgs[uid].labels {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

func (m *Mailbox) find(r provider.Ref) (*folder, uint32, bool) {
	f, ok := m.folders[r.Folder]
	if !ok {
		return nil, 0, false
	}
	v, u, _ := strings.Cut(r.ID, ".")
	if v != strconv.FormatUint(uint64(f.uidValidity), decimal) {
		return nil, 0, false
	}
	n, err := strconv.ParseUint(u, 10, 32)
	if err != nil {
		return nil, 0, false
	}
	if _, ok := f.msgs[uint32(n)]; !ok {
		return nil, 0, false
	}
	return f, uint32(n), true
}

func (m *Mailbox) gate() error {
	if m.Timeout {
		return fmt.Errorf("%w: timed out", provider.ErrUnavailable)
	}
	if m.FailSignIn {
		return provider.ErrAuth
	}
	return nil
}

// Check implements provider.Provider.
func (m *Mailbox) Check(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gate()
}

// Folders implements provider.Provider.
func (m *Mailbox) Folders(context.Context) ([]provider.Folder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.gate(); err != nil {
		return nil, err
	}
	var out []provider.Folder
	for _, f := range m.folders {
		out = append(out, provider.Folder{Ref: f.name, Name: f.name, Kind: provider.KindFor(f.name, ""), UIDValidity: f.uidValidity})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Changes implements provider.Provider with IMAP semantics: UIDs above the cursor's last
// UID, or everything since since after a UIDVALIDITY change or a stale cursor.
func (m *Mailbox) Changes(_ context.Context, fl provider.Folder, cur provider.Cursor, since time.Time, limit int) ([]provider.Ref, provider.Cursor, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.gate(); err != nil {
		return nil, cur, err
	}
	f, ok := m.folders[fl.Ref]
	if !ok {
		return nil, cur, nil
	}
	last := cur.LastUID
	if cur.UIDValidity != f.uidValidity || m.StaleCursor {
		last = zero
	}
	var uids []int
	for u, mm := range f.msgs {
		if u > last && !mm.date.Before(since) {
			uids = append(uids, int(u))
		}
	}
	sort.Ints(uids)
	if limit > zero && len(uids) > limit {
		uids = uids[:limit]
	}
	next := provider.Cursor{UIDValidity: f.uidValidity, LastUID: last}
	var out []provider.Ref
	for _, u := range uids {
		out = append(out, ref(f, uint32(u)))
		next.LastUID = uint32(u)
	}
	return out, next, nil
}

// Fetch implements provider.Provider. It never changes the read flag.
func (m *Mailbox) Fetch(_ context.Context, r provider.Ref) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.gate(); err != nil {
		return nil, err
	}
	m.Fetches[r.String()]++
	f, uid, ok := m.find(r)
	if !ok {
		return nil, provider.ErrNotFound
	}
	if m.dropOnFetch[r.String()] {
		delete(f.msgs, uid)
		return nil, provider.ErrNotFound
	}
	return append([]byte(nil), f.msgs[uid].raw...), nil
}

// AfterFile implements provider.Provider.
func (m *Mailbox) AfterFile(ctx context.Context, r provider.Ref, a provider.AfterFileAction) error {
	if a.MarkRead {
		if err := m.MarkRead(ctx, r, true); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.gate(); err != nil {
		return err
	}
	f, uid, ok := m.find(r)
	if !ok {
		return provider.ErrNotFound
	}
	switch a.Kind {
	case "leave":
	case "label":
		f.msgs[uid].labels[provider.GmailFiledLabel] = true
		m.Labeled = append(m.Labeled, r.String())
		m.Events = append(m.Events, "label "+r.String())
	case "move":
		m.moveLocked(f, uid, r, provider.FiledFolder)
	case "delete":
		delete(f.msgs, uid)
		m.Deleted = append(m.Deleted, r.String())
		m.Events = append(m.Events, "delete "+r.String())
	default:
		return errors.New("fake: unknown after-file action " + a.Kind)
	}
	return nil
}

func (m *Mailbox) moveLocked(f *folder, uid uint32, r provider.Ref, target string) provider.Ref {
	t := m.addFolderLocked(target)
	mm := f.msgs[uid]
	delete(f.msgs, uid)
	mm.uid = t.nextUID
	t.msgs[t.nextUID] = mm
	t.nextUID++
	m.Moved = append(m.Moved, r.String()+"->"+target)
	m.Events = append(m.Events, "move "+r.String()+" "+target)
	return ref(t, mm.uid)
}

// Move implements provider.Provider; the target folder is created when missing.
func (m *Mailbox) Move(_ context.Context, r provider.Ref, target string) (provider.Ref, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.gate(); err != nil {
		return provider.Ref{}, err
	}
	f, uid, ok := m.find(r)
	if !ok {
		return provider.Ref{}, provider.ErrNotFound
	}
	return m.moveLocked(f, uid, r, target), nil
}

// MarkRead implements provider.Provider.
func (m *Mailbox) MarkRead(_ context.Context, r provider.Ref, read bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.gate(); err != nil {
		return err
	}
	f, uid, ok := m.find(r)
	if !ok {
		return provider.ErrNotFound
	}
	f.msgs[uid].seen = read
	m.ReadChanges = append(m.ReadChanges, fmt.Sprintf("%s=%v", r.String(), read))
	m.Events = append(m.Events, fmt.Sprintf("read %s %v", r.String(), read))
	return nil
}

// Send implements provider.Provider and keeps the sent copy in Sent.
func (m *Mailbox) Send(_ context.Context, raw []byte, env provider.Envelope) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.gate(); err != nil {
		return "", err
	}
	if m.FailSend != nil {
		return "", m.FailSend
	}
	id := "fake-" + mailparse.Sum(raw)[:sentIDChars]
	m.sent = append(m.sent, Sent{Raw: append([]byte(nil), raw...), Env: env, ID: id})
	return id, nil
}

// Set is a fake for every account, keyed by address.
type Set struct {
	mu    sync.Mutex
	boxes map[string]*Mailbox
}

// NewSet returns an empty set.
func NewSet() *Set { return &Set{boxes: map[string]*Mailbox{}} }

// Box returns (creating) the mailbox for address.
func (s *Set) Box(address string) *Mailbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := strings.ToLower(address)
	if b, ok := s.boxes[a]; ok {
		return b
	}
	b := New(a)
	s.boxes[a] = b
	return b
}

// Factory returns a provider factory over the set; credentials are ignored, but the
// factory refuses any configured host other than 127.0.0.1, as every test helper does.
func (s *Set) Factory() func(ctx context.Context, c provider.Config) (provider.Provider, error) {
	return func(_ context.Context, c provider.Config) (provider.Provider, error) {
		for _, h := range []string{c.IMAPHost, c.SMTPHost} {
			if h != "" && h != "127.0.0.1" {
				return nil, errors.New("fake: test helpers connect to 127.0.0.1 only, not " + h)
			}
		}
		return s.Box(c.Address), nil
	}
}
