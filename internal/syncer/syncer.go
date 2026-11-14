package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/config"
	"github.com/CharitonMedia/togavsu-email/internal/coreapi"
	"github.com/CharitonMedia/togavsu-email/internal/filing"
	"github.com/CharitonMedia/togavsu-email/internal/mailparse"
	"github.com/CharitonMedia/togavsu-email/internal/provider"
	"github.com/CharitonMedia/togavsu-email/internal/render"
	"github.com/CharitonMedia/togavsu-email/internal/send"
	"github.com/CharitonMedia/togavsu-email/internal/store"
)

// RetryAfter is the wait after each failed fetch or parse (spec: 5 minutes, 30 minutes,
// 2 hours and 12 hours); after the last the row shows in the queue as error with Retry.
var RetryAfter = []time.Duration{five * minute, five * (five + one) * minute, two * hour, (ten + two) * hour}

// Syncer is the mail loop.
type Syncer struct {
	Store     *store.Store
	Core      *coreapi.Client
	Open      filing.Mailbox
	Processor *Processor
	Sender    *send.Sender
	Settings  func(ctx context.Context) (config.Settings, error)
	Now       func() time.Time
	Log       *slog.Logger

	mu          sync.Mutex
	kicked      bool
	lastRematch time.Time
	lastRun     map[string]time.Time
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Syncer) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Kick asks for a re-match of the queue on the next pass (a matter.* or contact.* event).
func (s *Syncer) Kick() {
	s.mu.Lock()
	s.kicked = true
	s.mu.Unlock()
}

// Run loops until ctx ends, one pass a minute; each account syncs when its poll
// interval has passed.
func (s *Syncer) Run(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		if err := s.Pass(ctx, false); err != nil && ctx.Err() == nil {
			s.log().Error("email sync pass", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Pass runs one loop pass: due accounts, the re-match when kicked or due, unfiled
// notices, the summary and the OAuth state purge. force syncs every account now.
func (s *Syncer) Pass(ctx context.Context, force bool) error {
	st, err := s.Settings(ctx)
	if err != nil {
		return err
	}
	if err := s.Store.PurgeStates(ctx); err != nil {
		return err
	}
	accts, err := s.Store.Accounts(ctx)
	if err != nil {
		return err
	}
	domains := s.domains(st, accts)
	s.mu.Lock()
	if s.lastRun == nil {
		s.lastRun = map[string]time.Time{}
	}
	s.mu.Unlock()
	var errs []error
	for _, a := range accts {
		if a.Status == "removed" {
			continue
		}
		s.mu.Lock()
		due := force || s.now().Sub(s.lastRun[a.ID]) >= st.Poll
		s.mu.Unlock()
		if !due {
			continue
		}
		if err := s.SyncAccount(ctx, a, st, domains); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.Address, err))
		}
		s.mu.Lock()
		s.lastRun[a.ID] = s.now()
		s.mu.Unlock()
	}
	s.mu.Lock()
	rematch := s.kicked || s.now().Sub(s.lastRematch) >= RematchEvery
	s.kicked = false
	s.mu.Unlock()
	if rematch {
		if err := s.Rematch(ctx, st, domains); err != nil {
			errs = append(errs, fmt.Errorf("rematch: %w", err))
		} else {
			s.mu.Lock()
			s.lastRematch = s.now()
			s.mu.Unlock()
		}
	}
	if err := s.NotifyUnfiled(ctx); err != nil {
		errs = append(errs, fmt.Errorf("unfiled notices: %w", err))
	}
	// A failed summary is retried next interval and never stops sync.
	if err := s.Summary(ctx, st); err != nil {
		s.log().Warn("email filing summary", "err", err)
	}
	return errors.Join(errs...)
}

func (s *Syncer) domains(st config.Settings, accts []store.Account) []string {
	var addrs []string
	for _, a := range accts {
		if a.Status != "removed" {
			addrs = append(addrs, a.Address)
		}
	}
	return st.Domains(addrs)
}

// Domains is the firm's mail domains now.
func (s *Syncer) Domains(ctx context.Context) ([]string, config.Settings, error) {
	st, err := s.Settings(ctx)
	if err != nil {
		return nil, st, err
	}
	accts, err := s.Store.Accounts(ctx)
	if err != nil {
		return nil, st, err
	}
	return s.domains(st, accts), st, nil
}

// wanted is whether a folder of kind is read on an account of role: personal accounts
// read Inbox and Sent; filing and court accounts read Inbox and subfolders, never Sent.
// Skip-list, Filed and Review-Later folders are never read.
func wanted(role, kind string) bool {
	switch kind {
	case provider.KindInbox:
		return true
	case provider.KindSent:
		return role == "personal"
	case provider.KindOther:
		return role != "personal"
	}
	return false
}

// SyncAccount runs one account's sync, one run at a time under an advisory lock.
func (s *Syncer) SyncAccount(ctx context.Context, a store.Account, st config.Settings, domains []string) error {
	unlock, ok, err := s.Store.TryLock(ctx, "account:"+a.ID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	defer unlock()
	// Re-read under the lock: another run may have changed the account.
	if a, err = s.Store.Account(ctx, a.ID); err != nil {
		return err
	}
	p, err := s.Open(ctx, a)
	if err == nil {
		err = p.Check(ctx)
	}
	if err != nil {
		return s.accountFailed(ctx, a, err)
	}
	if a.Status == "needs_reauth" || a.Status == "error" {
		if _, err := s.Store.SetStatus(ctx, a.ID, "connected", ""); err != nil {
			return err
		}
		a.Status = "connected"
	}
	folders, err := p.Folders(ctx)
	if err != nil {
		return s.accountFailed(ctx, a, err)
	}
	since := time.UnixMilli(a.SyncFrom)
	if a.SyncFrom == zero {
		since = s.now().Add(-st.Backfill)
	}
	for _, f := range folders {
		row, err := s.Store.UpsertFolder(ctx, a.ID, f.Ref, f.Name, f.Kind)
		if err != nil {
			return err
		}
		if !wanted(a.Role, f.Kind) {
			continue
		}
		if err := s.syncFolder(ctx, p, a, f, row, since, st, domains); err != nil {
			if errors.Is(err, provider.ErrAuth) || errors.Is(err, provider.ErrUnavailable) {
				return s.accountFailed(ctx, a, err)
			}
			return err
		}
	}
	if err := s.retries(ctx, p, a, st, domains); err != nil {
		return err
	}
	if a.Status != "paused" {
		paused, err := s.Store.PausedRows(ctx, a.ID)
		if err != nil {
			return err
		}
		for _, m := range paused {
			if _, err := s.Processor.Process(ctx, m, a, st, domains); err != nil {
				return err
			}
		}
	}
	return s.Store.MarkSynced(ctx, a.ID)
}

func (s *Syncer) syncFolder(ctx context.Context, p provider.Provider, a store.Account, f provider.Folder, row store.Folder, since time.Time,
	st config.Settings, domains []string) error {
	cur := provider.Cursor{UIDValidity: uint32(row.UIDValidity), LastUID: uint32(row.LastUID), Token: row.Cursor}
	refs, next, err := p.Changes(ctx, f, cur, since, BatchLimit)
	if err != nil {
		return err
	}
	for _, r := range refs {
		if err := s.take(ctx, p, a, row, r, st, domains); err != nil {
			return err
		}
	}
	return s.Store.SaveCursor(ctx, row.ID, int64(next.UIDValidity), int64(next.LastUID), next.Token)
}

func direction(a store.Account, kind string, pm *mailparse.Message, domains []string) string {
	if a.Role == "court" {
		return "court"
	}
	if kind == provider.KindSent {
		return "outbound"
	}
	if len(pm.From) == 0 {
		return "inbound"
	}
	for _, f := range pm.From {
		at := strings.LastIndexByte(f, '@')
		firm := false
		for _, d := range domains {
			firm = firm || (at >= zero && strings.EqualFold(f[at+one:], d))
		}
		if !firm {
			return "inbound"
		}
	}
	return "outbound"
}

// take reads one new message: fetch (never marking it read), parse, claim by its unique
// key, then match and file.
func (s *Syncer) take(ctx context.Context, p provider.Provider, a store.Account, folder store.Folder, r provider.Ref, st config.Settings, domains []string) error {
	ref := r.String()
	if known, err := s.Store.Known(ctx, a.ID, ref); err != nil || known {
		return err
	}
	raw, err := p.Fetch(ctx, r)
	if errors.Is(err, provider.ErrNotFound) {
		// Removed between list and fetch: skipped, and the cursor moves past it.
		return nil
	}
	if errors.Is(err, provider.ErrAuth) {
		return err
	}
	var pm *mailparse.Message
	if err == nil {
		pm, err = mailparse.Parse(raw)
	}
	if err != nil {
		return s.failedFetch(ctx, a, folder, ref, err)
	}
	// A folder renumbered by a UIDVALIDITY change holds messages already stored.
	if same, err := s.Store.SameMessage(ctx, a.ID, pm.MessageID, pm.RawSHA256); err != nil {
		return err
	} else if same != "" {
		return s.Store.MoveRef(ctx, same, folder.ID, ref)
	}
	m := Record(pm, a, folder, ref, direction(a, folder.Kind, pm, domains))
	won, err := s.Store.Claim(ctx, &m)
	if err != nil || !won {
		return err
	}
	if err := s.Store.SetParts(ctx, m.ID, send.PartsOf(pm)); err != nil {
		return err
	}
	stored, err := s.Store.Message(ctx, m.ID)
	if err != nil {
		return err
	}
	_, err = s.Processor.Process(ctx, stored, a, st, domains)
	return err
}

// Record is the stored mail record for a parsed message.
func Record(pm *mailparse.Message, a store.Account, folder store.Folder, ref, dir string) store.Message {
	m := send.RecordFromParsed(pm, a.ID, ref, dir)
	m.FolderID = folder.ID
	m.BodyHTML = render.Sanitize(pm.HTML)
	if strings.TrimSpace(m.BodyText) == "" {
		m.Snippet = mailparse.Snippet(render.Text(m.BodyHTML))
	}
	m.Unread = folder.Kind == provider.KindInbox
	return m
}

// failedFetch stores a row for a message that couldn't be read and schedules its retry.
func (s *Syncer) failedFetch(ctx context.Context, a store.Account, folder store.Folder, ref string, cause error) error {
	next := s.now().Add(RetryAfter[0]).UnixMilli()
	m := store.Message{AccountID: a.ID, FolderID: folder.ID, ProviderRef: ref, RFCMessageID: "<pending-" + ref + "@togavsu.invalid>",
		Status: "pending", Attempts: one, NextAttemptAt: &next, Error: cause.Error(), Subject: "(not read yet)"}
	_, err := s.Store.Claim(ctx, &m)
	return err
}

// retries fetches rows whose retry is due.
func (s *Syncer) retries(ctx context.Context, p provider.Provider, a store.Account, st config.Settings, domains []string) error {
	due, err := s.Store.DueRetries(ctx, a.ID)
	if err != nil {
		return err
	}
	for _, m := range due {
		if err := s.Reread(ctx, p, a, m, st, domains); err != nil {
			return err
		}
	}
	return nil
}

// Reread fetches and parses a failed row again; a failure schedules the next retry or,
// after the last, leaves the row as error.
func (s *Syncer) Reread(ctx context.Context, p provider.Provider, a store.Account, m store.Message, st config.Settings, domains []string) error {
	raw, err := p.Fetch(ctx, provider.ParseRef(m.ProviderRef))
	var pm *mailparse.Message
	if err == nil {
		pm, err = mailparse.Parse(raw)
	}
	if err != nil {
		if errors.Is(err, provider.ErrAuth) {
			return err
		}
		var next *int64
		if m.Attempts < len(RetryAfter) {
			t := s.now().Add(RetryAfter[m.Attempts]).UnixMilli()
			next = &t
		}
		return s.Store.Retry(ctx, m.ID, m.Attempts+1, next, err.Error())
	}
	kind := m.FolderKind
	rec := Record(pm, a, store.Folder{ID: m.FolderID, Kind: kind}, m.ProviderRef, direction(a, kind, pm, domains))
	rec.ID = m.ID
	if err := s.Store.SetParsed(ctx, rec); err != nil {
		return err
	}
	if err := s.Store.SetMessageStatus(ctx, m.ID, "pending", "", ""); err != nil {
		return err
	}
	if err := s.Store.SetParts(ctx, m.ID, send.PartsOf(pm)); err != nil {
		return err
	}
	stored, err := s.Store.Message(ctx, m.ID)
	if err != nil {
		return err
	}
	_, err = s.Processor.Process(ctx, stored, a, st, domains)
	return err
}

// accountFailed records a failing account. A sign-in failure sets needs_reauth and
// sends one account_problem notice, and no second one until the account recovers.
func (s *Syncer) accountFailed(ctx context.Context, a store.Account, cause error) error {
	status := "error"
	if errors.Is(cause, provider.ErrAuth) {
		status = "needs_reauth"
	}
	prev, err := s.Store.SetStatus(ctx, a.ID, status, cause.Error())
	if err != nil {
		return err
	}
	if status == "needs_reauth" && prev != "needs_reauth" {
		to := a.OwnerUserID
		if a.Role != "personal" {
			to = a.CreatedBy
		}
		key := "acct-" + a.ID + "-" + fmt.Sprint(s.now().UnixMilli())
		if err := s.Core.Notify(ctx, key, to, "account_problem", "Mail account "+a.Address+" needs to be connected again",
			"/modules/email/tabs/accounts"); err != nil {
			s.log().Warn("account_problem notification", "err", err)
		}
	}
	return nil
}

// Rematch runs every unfiled row through the rules again, under one advisory lock.
func (s *Syncer) Rematch(ctx context.Context, st config.Settings, domains []string) error {
	unlock, ok, err := s.Store.TryLock(ctx, "rematch")
	if err != nil || !ok {
		return err
	}
	defer unlock()
	rows, err := s.Store.Unfiled(ctx)
	if err != nil {
		return err
	}
	accts := map[string]store.Account{}
	for _, m := range rows {
		a, ok := accts[m.AccountID]
		if !ok {
			if a, err = s.Store.Account(ctx, m.AccountID); err != nil {
				return err
			}
			accts[m.AccountID] = a
		}
		if a.Status == "paused" || a.Status == "removed" || m.Error == filing.NotInMailbox {
			continue
		}
		if _, err := s.Processor.Process(ctx, m, a, st, domains); err != nil {
			return err
		}
	}
	return nil
}

// NotifyUnfiled sends unfiled_mail notices for rows that arrived since each person's
// last notice, at most once every 15 minutes per person: the owner of a personal
// account; the admin who connected a firm or court account, and everyone with
// notify_unfiled on.
func (s *Syncer) NotifyUnfiled(ctx context.Context) error {
	rows, err := s.Store.NewUnfiled(ctx)
	if err != nil || len(rows) == 0 {
		return err
	}
	opted, err := s.Store.NotifyUnfiledUsers(ctx)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, m := range rows {
		if m.Role == "personal" {
			counts[m.OwnerUserID]++
			continue
		}
		counts[m.AccountCreatedBy]++
		for _, u := range opted {
			if u != m.AccountCreatedBy {
				counts[u]++
			}
		}
	}
	users := make([]string, 0, len(counts))
	for u := range counts {
		users = append(users, u)
	}
	sort.Strings(users)
	for _, u := range users {
		ok, err := s.Store.ClaimUnfiledNotice(ctx, u, UnfiledNoticeGap)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		title := "1 email waits to be filed"
		if counts[u] > one {
			title = fmt.Sprintf("%d emails wait to be filed", counts[u])
		}
		if err := s.Core.Notify(ctx, "unfiled-"+u+"-"+fmt.Sprint(s.now().UnixMilli()), u, "unfiled_mail", title, "/modules/email/tabs/unfiled"); err != nil {
			s.log().Warn("unfiled_mail notification", "err", err)
		}
	}
	return s.Store.MarkUnfiledNotified(ctx, rows)
}

// Summary sends the filing summary when it is due: at most once per interval, only
// when something was filed, listing filings by matter and the unfiled count.
func (s *Syncer) Summary(ctx context.Context, st config.Settings) error {
	if len(st.SummaryRecipients) == 0 || s.Sender == nil {
		return nil
	}
	fs, err := s.Store.Unreported(ctx)
	if err != nil || len(fs) == 0 {
		return err
	}
	now := s.now().UnixMilli()
	last, err := s.Store.LastSummaryAt(ctx)
	if err != nil {
		return err
	}
	iv := st.SummaryInterval.Milliseconds()
	if now-last < iv || now-fs[0].FiledAt < iv {
		return nil
	}
	unfiled, err := s.Store.Queue(ctx, store.QueueFilter{Statuses: []string{"unfiled", "error"}, Firm: true})
	if err != nil {
		return err
	}
	byMatter := map[string][]store.Filing{}
	var matters []string
	for _, f := range fs {
		if _, ok := byMatter[f.MatterID]; !ok {
			matters = append(matters, f.MatterID)
		}
		byMatter[f.MatterID] = append(byMatter[f.MatterID], f)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d emails filed.\n\n", len(fs))
	// matchMail with known ids is the one matter read a service call has; a walled
	// matter doesn't come back and is listed without its name.
	labels := map[string]string{}
	if ans, err := s.Core.MatchMail(ctx, coreapi.Service, coreapi.MatchRequest{KnownMatterIDs: matters}); err == nil {
		for _, m := range ans.Matters {
			labels[m.ID] = m.Number + " " + m.Name
		}
	}
	var idsDone []string
	for _, mid := range matters {
		label, ok := labels[mid]
		if !ok {
			label = "A matter you may not see here"
		}
		fmt.Fprintf(&b, "%s\n", label)
		for _, f := range byMatter[mid] {
			fmt.Fprintf(&b, "  - %s (%s)\n", f.Subject, f.FromAddr)
			idsDone = append(idsDone, f.MessageID)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Unfiled email waiting: %d\n", len(unfiled))
	if err := s.Sender.Plain(ctx, "filing_summary", st.SummaryRecipients, fmt.Sprintf("Email filing summary: %d filed", len(fs)), b.String()); err != nil {
		return err
	}
	return s.Store.RecordSummary(ctx, strings.Join(st.SummaryRecipients, ","), idsDone, len(unfiled))
}
