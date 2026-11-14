// Package store reads and writes the module's own schema, mod_email, through the
// module's own database login. It never touches a core table.
package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/ids"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrNotFound is a row that isn't there (or that the viewer may not see).
var ErrNotFound = errors.New("store: not found")

// Store is the module's schema.
type Store struct {
	DB  *sql.DB
	Now func() time.Time
}

// New returns a store over db.
func New(db *sql.DB, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{DB: db, Now: now}
}

func (s *Store) ms() int64 { return s.Now().UnixMilli() }

var types = pgtype.NewMap()

func arr(dst *[]string) sql.Scanner { return types.SQLScanner(dst) }

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

type scanner interface{ Scan(...any) error }

// --- accounts ---

// Account is one connected mailbox.
type Account struct {
	ID             string `json:"id"`
	Provider       string `json:"provider"`
	Role           string `json:"role"`
	Address        string `json:"address"`
	Label          string `json:"label"`
	OwnerUserID    string `json:"owner_user_id,omitempty"`
	CreatedBy      string `json:"created_by"`
	IMAPHost       string `json:"imap_host,omitempty"`
	IMAPPort       int    `json:"imap_port,omitempty"`
	IMAPSecurity   string `json:"imap_security,omitempty"`
	SMTPHost       string `json:"smtp_host,omitempty"`
	SMTPPort       int    `json:"smtp_port,omitempty"`
	SMTPSecurity   string `json:"smtp_security,omitempty"`
	Login          string `json:"login,omitempty"`
	SecretRef      string `json:"-"`
	CanSend        bool   `json:"can_send"`
	DefaultSend    bool   `json:"default_send"`
	SystemMail     bool   `json:"system_mail"`
	PlusAddressing bool   `json:"plus_addressing"`
	AfterFile      string `json:"after_file"`
	Status         string `json:"status"`
	LastSyncAt     *int64 `json:"last_sync_at"`
	LastError      string `json:"last_error"`
	SyncFrom       int64  `json:"sync_from"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
}

const accountCols = `id, provider, role, address, label, COALESCE(owner_user_id, ''), created_by, imap_host, imap_port, imap_security,
smtp_host, smtp_port, smtp_security, login, secret_ref, can_send, default_send, system_mail, plus_addressing, after_file, status,
last_sync_at, last_error, sync_from, created_at, updated_at`

func scanAccount(r scanner) (Account, error) {
	var a Account
	var last sql.NullInt64
	err := r.Scan(&a.ID, &a.Provider, &a.Role, &a.Address, &a.Label, &a.OwnerUserID, &a.CreatedBy, &a.IMAPHost, &a.IMAPPort, &a.IMAPSecurity,
		&a.SMTPHost, &a.SMTPPort, &a.SMTPSecurity, &a.Login, &a.SecretRef, &a.CanSend, &a.DefaultSend, &a.SystemMail, &a.PlusAddressing,
		&a.AfterFile, &a.Status, &last, &a.LastError, &a.SyncFrom, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if last.Valid {
		a.LastSyncAt = &last.Int64
	}
	return a, err
}

// InsertAccount stores a new account. The database checks refuse a court account that
// deletes, and a personal account with no owner.
func (s *Store) InsertAccount(ctx context.Context, a *Account) error {
	now := s.ms()
	if a.ID == "" {
		a.ID = ids.New(s.Now())
	}
	a.CreatedAt, a.UpdatedAt = now, now
	if a.Status == "" {
		a.Status = "connected"
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO accounts (id, provider, role, address, label, owner_user_id, created_by, imap_host, imap_port,
imap_security, smtp_host, smtp_port, smtp_security, login, secret_ref, can_send, default_send, system_mail, plus_addressing, after_file,
status, sync_from, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)`,
		a.ID, a.Provider, a.Role, strings.ToLower(a.Address), a.Label, nullStr(a.OwnerUserID), a.CreatedBy, a.IMAPHost, a.IMAPPort, orDefault(a.IMAPSecurity, "tls"),
		a.SMTPHost, a.SMTPPort, orDefault(a.SMTPSecurity, "starttls"), a.Login, a.SecretRef, a.CanSend, a.DefaultSend, a.SystemMail, a.PlusAddressing,
		a.AfterFile, a.Status, a.SyncFrom, now, now)
	return err
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// Account reads one account.
func (s *Store) Account(ctx context.Context, id string) (Account, error) {
	return scanAccount(s.DB.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE id = $1`, id))
}

// Accounts lists every account that isn't removed.
func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE status <> 'removed' ORDER BY role, address`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SaveAccount writes an account's editable fields.
func (s *Store) SaveAccount(ctx context.Context, a Account) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE accounts SET label=$2, can_send=$3, default_send=$4, system_mail=$5, plus_addressing=$6,
after_file=$7, status=$8, secret_ref=$9, address=$10, updated_at=$11 WHERE id=$1`,
		a.ID, a.Label, a.CanSend, a.DefaultSend, a.SystemMail, a.PlusAddressing, a.AfterFile, a.Status, a.SecretRef, strings.ToLower(a.Address), s.ms())
	return err
}

// SetStatus changes an account's status and returns the status it had.
func (s *Store) SetStatus(ctx context.Context, id, status, lastErr string) (string, error) {
	var prev string
	err := s.DB.QueryRowContext(ctx, `UPDATE accounts a SET status = $2, last_error = $3, updated_at = $4
FROM (SELECT status FROM accounts WHERE id = $1 FOR UPDATE) old WHERE a.id = $1 RETURNING old.status`, id, status, lastErr, s.ms()).Scan(&prev)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return prev, err
}

// MarkSynced stamps a finished sync.
func (s *Store) MarkSynced(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE accounts SET last_sync_at = $2 WHERE id = $1`, id, s.ms())
	return err
}

// SystemMailAccount is the account marked for system mail.
func (s *Store) SystemMailAccount(ctx context.Context) (Account, error) {
	return scanAccount(s.DB.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE system_mail AND status <> 'removed'`))
}

// DefaultSendAccount is the account marked default_send.
func (s *Store) DefaultSendAccount(ctx context.Context) (Account, error) {
	return scanAccount(s.DB.QueryRowContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE default_send AND can_send AND status = 'connected'`))
}

// PersonalAccounts lists a user's own personal accounts.
func (s *Store) PersonalAccounts(ctx context.Context, userID string) ([]Account, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+accountCols+` FROM accounts WHERE role = 'personal' AND owner_user_id = $1 AND status <> 'removed' ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// --- folders ---

// Folder is a stored folder with its sync position.
type Folder struct {
	ID          string
	AccountID   string
	ProviderRef string
	Name        string
	Kind        string
	UIDValidity int64
	LastUID     int64
	Cursor      string
}

// UpsertFolder records a provider folder and returns the stored row.
func (s *Store) UpsertFolder(ctx context.Context, accountID, ref, name, kind string) (Folder, error) {
	f := Folder{AccountID: accountID, ProviderRef: ref}
	err := s.DB.QueryRowContext(ctx, `INSERT INTO folders (id, account_id, provider_ref, name, kind, updated_at) VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (account_id, provider_ref) DO UPDATE SET name = EXCLUDED.name, kind = EXCLUDED.kind
RETURNING id, name, kind, uid_validity, last_uid, cursor`, ids.New(s.Now()), accountID, ref, name, kind, s.ms()).
		Scan(&f.ID, &f.Name, &f.Kind, &f.UIDValidity, &f.LastUID, &f.Cursor)
	return f, err
}

// SaveCursor stores a folder's sync position.
func (s *Store) SaveCursor(ctx context.Context, id string, uidValidity, lastUID int64, cursor string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE folders SET uid_validity=$2, last_uid=$3, cursor=$4, updated_at=$5 WHERE id=$1`, id, uidValidity, lastUID, cursor, s.ms())
	return err
}

// --- oauth states ---

// OAuthState is an OAuth consent in flight.
type OAuthState struct {
	State        string
	AccountID    string
	UserID       string
	Provider     string
	Role         string
	OwnerUserID  string
	Label        string
	CodeVerifier string
	RedirectURI  string
	ExpiresAt    int64
}

// InsertState stores a state for ten minutes.
func (s *Store) InsertState(ctx context.Context, st OAuthState) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO oauth_states (state, account_id, user_id, provider, role, owner_user_id, label, code_verifier, redirect_uri, expires_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, st.State, st.AccountID, st.UserID, st.Provider, st.Role, nullStr(st.OwnerUserID), st.Label, st.CodeVerifier, st.RedirectURI, st.ExpiresAt)
	return err
}

// TakeState removes and returns a state that hasn't expired.
func (s *Store) TakeState(ctx context.Context, state string) (OAuthState, error) {
	var st OAuthState
	err := s.DB.QueryRowContext(ctx, `DELETE FROM oauth_states WHERE state = $1 RETURNING state, account_id, user_id, provider, role,
COALESCE(owner_user_id, ''), label, code_verifier, redirect_uri, expires_at`, state).
		Scan(&st.State, &st.AccountID, &st.UserID, &st.Provider, &st.Role, &st.OwnerUserID, &st.Label, &st.CodeVerifier, &st.RedirectURI, &st.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) || err == nil && st.ExpiresAt < s.ms() {
		return OAuthState{}, ErrNotFound
	}
	return st, err
}

// PurgeStates deletes states older than ten minutes.
func (s *Store) PurgeStates(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM oauth_states WHERE expires_at < $1`, s.ms())
	return err
}

// --- user prefs ---

// Prefs is a user's preferences.
type Prefs struct {
	UserID        string `json:"user_id"`
	NotifyUnfiled bool   `json:"notify_unfiled"`
	SignatureText string `json:"signature_text"`
}

// Prefs reads a user's preferences (defaults when none are saved).
func (s *Store) Prefs(ctx context.Context, userID string) (Prefs, error) {
	p := Prefs{UserID: userID}
	err := s.DB.QueryRowContext(ctx, `SELECT notify_unfiled, signature_text FROM user_prefs WHERE user_id = $1`, userID).Scan(&p.NotifyUnfiled, &p.SignatureText)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	return p, err
}

// SavePrefs writes a user's preferences.
func (s *Store) SavePrefs(ctx context.Context, p Prefs) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO user_prefs (user_id, notify_unfiled, signature_text) VALUES ($1,$2,$3)
ON CONFLICT (user_id) DO UPDATE SET notify_unfiled = EXCLUDED.notify_unfiled, signature_text = EXCLUDED.signature_text`, p.UserID, p.NotifyUnfiled, p.SignatureText)
	return err
}

// NotifyUnfiledUsers lists users who asked to hear about every unfiled message.
func (s *Store) NotifyUnfiledUsers(ctx context.Context) ([]string, error) {
	return s.strings(ctx, `SELECT user_id FROM user_prefs WHERE notify_unfiled ORDER BY user_id`)
}

// ClaimUnfiledNotice reports whether user may get an unfiled notice now (at most one per
// gap) and stamps it.
func (s *Store) ClaimUnfiledNotice(ctx context.Context, userID string, gap time.Duration) (bool, error) {
	now := s.ms()
	res, err := s.DB.ExecContext(ctx, `INSERT INTO user_prefs (user_id, unfiled_notified_at) VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE SET unfiled_notified_at = EXCLUDED.unfiled_notified_at
WHERE user_prefs.unfiled_notified_at <= $3`, userID, now, now-gap.Milliseconds())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) strings(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// --- messages ---

// Message is a stored mail record.
type Message struct {
	ID            string   `json:"id"`
	AccountID     string   `json:"account_id"`
	FolderID      string   `json:"folder_id,omitempty"`
	ProviderRef   string   `json:"-"`
	RFCMessageID  string   `json:"rfc_message_id"`
	ThreadKey     string   `json:"thread_key"`
	InReplyTo     string   `json:"in_reply_to,omitempty"`
	ReferenceIDs  []string `json:"reference_ids,omitempty"`
	Direction     string   `json:"direction"`
	FromHeader    string   `json:"from_header"`
	FromAddr      string   `json:"from_addr"`
	ToAddrs       []string `json:"to_addrs"`
	CcAddrs       []string `json:"cc_addrs"`
	DeliveredTo   string   `json:"delivered_to,omitempty"`
	Subject       string   `json:"subject"`
	SentAt        *int64   `json:"sent_at"`
	ReceivedAt    int64    `json:"received_at"`
	Snippet       string   `json:"snippet"`
	BodyText      string   `json:"body_text,omitempty"`
	BodyHTML      string   `json:"body_html,omitempty"`
	RawSHA256     string   `json:"raw_sha256"`
	RawSize       int64    `json:"raw_size"`
	Unread        bool     `json:"unread"`
	Status        string   `json:"status"`
	MatchReason   string   `json:"match_reason"`
	AIDeclined    bool     `json:"ai_declined"`
	Attempts      int      `json:"attempts"`
	NextAttemptAt *int64   `json:"next_attempt_at,omitempty"`
	Error         string   `json:"error"`
	CreatedAt     int64    `json:"created_at"`
	// FolderKind is the stored folder's kind (joined), when known.
	FolderKind string `json:"folder_kind,omitempty"`
	// Role and OwnerUserID are the account's (joined).
	Role        string `json:"account_role"`
	OwnerUserID string `json:"-"`
}

const messageCols = `m.id, m.account_id, COALESCE(m.folder_id, ''), m.provider_ref, m.rfc_message_id, m.thread_key, m.in_reply_to, m.reference_ids,
m.direction, m.from_header, m.from_addr, m.to_addrs, m.cc_addrs, m.delivered_to, m.subject, m.sent_at, m.received_at, m.snippet, m.body_text,
m.body_html, m.raw_sha256, m.raw_size, m.unread, m.status, m.match_reason, m.ai_declined, m.attempts, m.next_attempt_at, m.error, m.created_at,
COALESCE(f.kind, ''), a.role, COALESCE(a.owner_user_id, '')`

const messageFrom = ` FROM messages m JOIN accounts a ON a.id = m.account_id LEFT JOIN folders f ON f.id = m.folder_id `

func scanMessage(r scanner) (Message, error) {
	var m Message
	var sent, next sql.NullInt64
	err := r.Scan(&m.ID, &m.AccountID, &m.FolderID, &m.ProviderRef, &m.RFCMessageID, &m.ThreadKey, &m.InReplyTo, arr(&m.ReferenceIDs),
		&m.Direction, &m.FromHeader, &m.FromAddr, arr(&m.ToAddrs), arr(&m.CcAddrs), &m.DeliveredTo, &m.Subject, &sent, &m.ReceivedAt, &m.Snippet,
		&m.BodyText, &m.BodyHTML, &m.RawSHA256, &m.RawSize, &m.Unread, &m.Status, &m.MatchReason, &m.AIDeclined, &m.Attempts, &next, &m.Error,
		&m.CreatedAt, &m.FolderKind, &m.Role, &m.OwnerUserID)
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	if sent.Valid {
		m.SentAt = &sent.Int64
	}
	if next.Valid {
		m.NextAttemptAt = &next.Int64
	}
	return m, err
}

// Claim inserts a message row by its unique key (account, provider ref) before any
// filing write. It returns false when another run already holds that message.
func (s *Store) Claim(ctx context.Context, m *Message) (bool, error) {
	now := s.ms()
	if m.ID == "" {
		m.ID = ids.New(s.Now())
	}
	if m.ReceivedAt == 0 {
		m.ReceivedAt = now
	}
	if m.ReferenceIDs == nil {
		m.ReferenceIDs = []string{}
	}
	if m.ToAddrs == nil {
		m.ToAddrs = []string{}
	}
	if m.CcAddrs == nil {
		m.CcAddrs = []string{}
	}
	var got string
	err := s.DB.QueryRowContext(ctx, `INSERT INTO messages (id, account_id, folder_id, provider_ref, rfc_message_id, thread_key, in_reply_to,
reference_ids, direction, from_header, from_addr, to_addrs, cc_addrs, delivered_to, subject, sent_at, received_at, snippet, body_text, body_html,
raw_sha256, raw_size, unread, status, match_reason, attempts, next_attempt_at, error, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$29)
ON CONFLICT (account_id, provider_ref) DO NOTHING RETURNING id`,
		m.ID, m.AccountID, nullStr(m.FolderID), m.ProviderRef, m.RFCMessageID, m.ThreadKey, m.InReplyTo, m.ReferenceIDs, orDefault(m.Direction, "inbound"),
		m.FromHeader, m.FromAddr, m.ToAddrs, m.CcAddrs, m.DeliveredTo, m.Subject, m.SentAt, m.ReceivedAt, m.Snippet, m.BodyText, m.BodyHTML,
		m.RawSHA256, m.RawSize, m.Unread, orDefault(m.Status, "pending"), m.MatchReason, m.Attempts, m.NextAttemptAt, m.Error, now).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Known reports whether the account already holds a row for provider ref.
func (s *Store) Known(ctx context.Context, accountID, ref string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE account_id = $1 AND provider_ref = $2`, accountID, ref).Scan(&n)
	return n > 0, err
}

// SameMessage finds a row in the account with the same Message-ID and raw bytes (a
// folder renumbered by a UIDVALIDITY change).
func (s *Store) SameMessage(ctx context.Context, accountID, rfcID, sha string) (string, error) {
	var id string
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM messages WHERE account_id = $1 AND rfc_message_id = $2 AND raw_sha256 = $3 LIMIT 1`, accountID, rfcID, sha).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// MoveRef points a row at the provider's new ref for the same message.
func (s *Store) MoveRef(ctx context.Context, id, folderID, ref string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE messages SET provider_ref = $2, folder_id = $3, updated_at = $4 WHERE id = $1`, id, ref, nullStr(folderID), s.ms())
	return err
}

// Message reads one message.
func (s *Store) Message(ctx context.Context, id string) (Message, error) {
	return scanMessage(s.DB.QueryRowContext(ctx, `SELECT `+messageCols+messageFrom+` WHERE m.id = $1`, id))
}

// MessageByRFC reads the newest row with a Message-ID, preferring one that is filed.
func (s *Store) MessageByRFC(ctx context.Context, rfcID string) (Message, error) {
	return scanMessage(s.DB.QueryRowContext(ctx, `SELECT `+messageCols+messageFrom+` WHERE m.rfc_message_id = $1
ORDER BY (m.status = 'filed') DESC, m.created_at DESC LIMIT 1`, rfcID))
}

// SetParsed stores what the parser read on a claimed row.
func (s *Store) SetParsed(ctx context.Context, m Message) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE messages SET rfc_message_id=$2, thread_key=$3, in_reply_to=$4, reference_ids=$5, direction=$6,
from_header=$7, from_addr=$8, to_addrs=$9, cc_addrs=$10, delivered_to=$11, subject=$12, sent_at=$13, snippet=$14, body_text=$15, body_html=$16,
raw_sha256=$17, raw_size=$18, error='', next_attempt_at=NULL, updated_at=$19 WHERE id=$1`,
		m.ID, m.RFCMessageID, m.ThreadKey, m.InReplyTo, nonNil(m.ReferenceIDs), m.Direction, m.FromHeader, m.FromAddr, nonNil(m.ToAddrs), nonNil(m.CcAddrs),
		m.DeliveredTo, m.Subject, m.SentAt, m.Snippet, m.BodyText, m.BodyHTML, m.RawSHA256, m.RawSize, s.ms())
	return err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// SetStatus sets a message's status, reason and error.
func (s *Store) SetMessageStatus(ctx context.Context, id, status, reason, errText string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE messages SET status=$2, match_reason=$3, error=$4, updated_at=$5 WHERE id=$1`, id, status, reason, errText, s.ms())
	return err
}

// SetAIDeclined records that the model said none for this message.
func (s *Store) SetAIDeclined(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE messages SET ai_declined = true WHERE id = $1`, id)
	return err
}

// SetUnread stores the read flag shown in the inbox.
func (s *Store) SetUnread(ctx context.Context, id string, unread bool) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE messages SET unread = $2 WHERE id = $1`, id, unread)
	return err
}

// Retry schedules another fetch after a failure, or marks the row error after the last.
func (s *Store) Retry(ctx context.Context, id string, attempts int, next *int64, errText string) error {
	status := "pending"
	if next == nil {
		status = "error"
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE messages SET attempts=$2, next_attempt_at=$3, error=$4, status=$5, updated_at=$6 WHERE id=$1`,
		id, attempts, next, errText, status, s.ms())
	return err
}

// DueRetries lists pending rows of an account whose next attempt is due.
func (s *Store) DueRetries(ctx context.Context, accountID string) ([]Message, error) {
	return s.messages(ctx, ` WHERE m.account_id = $1 AND m.status = 'pending' AND m.next_attempt_at IS NOT NULL AND m.next_attempt_at <= $2 ORDER BY m.created_at`, accountID, s.ms())
}

// Unfiled lists every row waiting in the queue for a re-match.
func (s *Store) Unfiled(ctx context.Context) ([]Message, error) {
	return s.messages(ctx, ` WHERE m.status = 'unfiled' AND a.status <> 'removed' ORDER BY m.received_at`)
}

func (s *Store) messages(ctx context.Context, where string, args ...any) ([]Message, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+messageCols+messageFrom+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// QueueFilter selects queue rows.
type QueueFilter struct {
	Statuses  []string
	AccountID string
	// Firm includes filing and court account rows.
	Firm bool
	// Owner includes that user's personal account rows.
	Owner string
	Limit int
}

// Queue lists queue rows oldest first.
func (s *Store) Queue(ctx context.Context, f QueueFilter) ([]Message, error) {
	where := ` WHERE m.status = ANY($1) AND a.status <> 'removed' AND (($2 AND a.role IN ('filing', 'court')) OR (a.role = 'personal' AND a.owner_user_id = $3))`
	args := []any{f.Statuses, f.Firm, f.Owner}
	if f.AccountID != "" {
		where += ` AND m.account_id = $4`
		args = append(args, f.AccountID)
	}
	where += ` ORDER BY m.received_at, m.id`
	if f.Limit > 0 {
		where += ` LIMIT ` + itoa(f.Limit)
	}
	return s.messages(ctx, where, args...)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Inbox lists a personal account folder's rows, newest first.
func (s *Store) Inbox(ctx context.Context, accountIDs []string, kind string, limit int) ([]Message, error) {
	return s.messages(ctx, ` WHERE m.account_id = ANY($1) AND f.kind = $2 AND m.status NOT IN ('error') ORDER BY m.received_at DESC LIMIT `+itoa(limit), accountIDs, kind)
}

// UnreadCount counts unread inbox rows.
func (s *Store) UnreadCount(ctx context.Context, accountIDs []string) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM messages m JOIN folders f ON f.id = m.folder_id WHERE m.account_id = ANY($1) AND f.kind = 'inbox' AND m.unread`, accountIDs).Scan(&n)
	return n, err
}

// Search finds messages by subject, from, to and body text.
func (s *Store) Search(ctx context.Context, q string, limit int) ([]Message, error) {
	like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(q)) + "%"
	return s.messages(ctx, ` WHERE m.status <> 'error' AND (lower(m.subject) LIKE $1 OR lower(m.from_header) LIKE $1 OR lower(m.from_addr) LIKE $1
OR lower(array_to_string(m.to_addrs, ' ')) LIKE $1 OR lower(m.body_text) LIKE $1) ORDER BY m.received_at DESC LIMIT `+itoa(limit), like)
}

// --- parts, contacts, matches ---

// Part is a stored part.
type Part struct {
	Index     int    `json:"part_index"`
	Filename  string `json:"filename"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	Inline    bool   `json:"inline"`
	ContentID string `json:"content_id,omitempty"`
	FileID    string `json:"file_id,omitempty"`
}

// SetParts replaces a message's parts list.
func (s *Store) SetParts(ctx context.Context, messageID string, parts []Part) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_parts WHERE message_id = $1 AND file_id IS NULL`, messageID); err != nil {
		return err
	}
	for _, p := range parts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_parts (id, message_id, part_index, filename, media_type, size_bytes, sha256, inline, content_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (message_id, part_index) DO NOTHING`, ids.New(s.Now()), messageID, p.Index, p.Filename, p.MediaType, p.Size, p.SHA256, p.Inline, p.ContentID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Parts lists a message's parts.
func (s *Store) Parts(ctx context.Context, messageID string) ([]Part, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT part_index, filename, media_type, size_bytes, sha256, inline, content_id, COALESCE(file_id, '')
FROM message_parts WHERE message_id = $1 ORDER BY part_index`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Part
	for rows.Next() {
		var p Part
		if err := rows.Scan(&p.Index, &p.Filename, &p.MediaType, &p.Size, &p.SHA256, &p.Inline, &p.ContentID, &p.FileID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPartFile records the core file an attachment was filed as.
func (s *Store) SetPartFile(ctx context.Context, messageID string, index int, fileID string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE message_parts SET file_id = $3 WHERE message_id = $1 AND part_index = $2`, messageID, index, fileID)
	return err
}

// SetContacts replaces the contacts behind a message's addresses.
func (s *Store) SetContacts(ctx context.Context, messageID string, byAddr map[string]string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_contacts WHERE message_id = $1`, messageID); err != nil {
		return err
	}
	for contact, addr := range byAddr {
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_contacts (message_id, contact_id, address) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, messageID, contact, addr); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Match is a candidate matter saved for the queue.
type Match struct {
	MatterID string `json:"matter_id"`
	Via      string `json:"via"`
	Rank     int    `json:"rank"`
}

// SetMatches replaces a message's candidates.
func (s *Store) SetMatches(ctx context.Context, messageID string, ms []Match) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM message_matches WHERE message_id = $1`, messageID); err != nil {
		return err
	}
	for _, m := range ms {
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_matches (message_id, matter_id, via, rank) VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`, messageID, m.MatterID, m.Via, m.Rank); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Matches lists a message's candidates by rank.
func (s *Store) Matches(ctx context.Context, messageID string) ([]Match, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT matter_id, via, rank FROM message_matches WHERE message_id = $1 ORDER BY rank, matter_id`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Match
	for rows.Next() {
		var m Match
		if err := rows.Scan(&m.MatterID, &m.Via, &m.Rank); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// --- filings ---

// Filing is one filed message.
type Filing struct {
	MessageID    string `json:"message_id"`
	MatterID     string `json:"matter_id"`
	RFCMessageID string `json:"rfc_message_id"`
	NoteID       string `json:"note_id"`
	HTMLFileID   string `json:"html_file_id"`
	FiledBy      string `json:"filed_by,omitempty"`
	How          string `json:"how"`
	AttorneyOnly bool   `json:"attorney_only"`
	FiledAt      int64  `json:"filed_at"`
	// Joined from the message.
	Subject   string `json:"subject"`
	FromAddr  string `json:"from_addr"`
	Direction string `json:"direction"`
	ThreadKey string `json:"thread_key"`
	SentAt    *int64 `json:"sent_at"`
}

// FiledMatter returns the matter a Message-ID is filed to, if any.
func (s *Store) FiledMatter(ctx context.Context, rfcID string) (string, error) {
	var m string
	err := s.DB.QueryRowContext(ctx, `SELECT matter_id FROM filings WHERE rfc_message_id = $1 ORDER BY filed_at LIMIT 1`, rfcID).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return m, err
}

// ThreadMatters returns the matters any of rfcIDs are filed to.
func (s *Store) ThreadMatters(ctx context.Context, rfcIDs []string) ([]string, error) {
	if len(rfcIDs) == 0 {
		return nil, nil
	}
	return s.strings(ctx, `SELECT DISTINCT matter_id FROM filings WHERE rfc_message_id = ANY($1) ORDER BY matter_id`, rfcIDs)
}

// RecordFiling writes the filing, the matter's mail extension, the message's status
// and, for a filing by a person, the queue action, in one transaction.
func (s *Store) RecordFiling(ctx context.Context, f Filing, direction string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.ms()
	if _, err := tx.ExecContext(ctx, `INSERT INTO filings (message_id, matter_id, rfc_message_id, note_id, html_file_id, filed_by, how, attorney_only, filed_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (message_id) DO NOTHING`, f.MessageID, f.MatterID, f.RFCMessageID, f.NoteID, f.HTMLFileID, nullStr(f.FiledBy), f.How, f.AttorneyOnly, now); err != nil {
		return err
	}
	inbound := any(nil)
	if direction != "outbound" {
		inbound = now
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO matter_mail (matter_id, filed_count, last_mail_at, last_inbound_at) VALUES ($1, 1, $2, $3)
ON CONFLICT (matter_id) DO UPDATE SET filed_count = matter_mail.filed_count + 1, last_mail_at = EXCLUDED.last_mail_at,
last_inbound_at = COALESCE(EXCLUDED.last_inbound_at, matter_mail.last_inbound_at)`, f.MatterID, now, inbound); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET status = 'filed', error = '', updated_at = $2 WHERE id = $1`, f.MessageID, now); err != nil {
		return err
	}
	if f.FiledBy != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO queue_actions (id, message_id, action, user_id, at) VALUES ($1,$2,'file',$3,$4)`, ids.New(s.Now()), f.MessageID, f.FiledBy, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const filingCols = `f.message_id, f.matter_id, f.rfc_message_id, f.note_id, f.html_file_id, COALESCE(f.filed_by, ''), f.how, f.attorney_only, f.filed_at,
m.subject, m.from_addr, m.direction, m.thread_key, m.sent_at`

func scanFilings(rows *sql.Rows) ([]Filing, error) {
	defer rows.Close()
	var out []Filing
	for rows.Next() {
		var f Filing
		var sent sql.NullInt64
		if err := rows.Scan(&f.MessageID, &f.MatterID, &f.RFCMessageID, &f.NoteID, &f.HTMLFileID, &f.FiledBy, &f.How, &f.AttorneyOnly, &f.FiledAt,
			&f.Subject, &f.FromAddr, &f.Direction, &f.ThreadKey, &sent); err != nil {
			return nil, err
		}
		if sent.Valid {
			f.SentAt = &sent.Int64
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Filing reads the filing of one message.
func (s *Store) Filing(ctx context.Context, messageID string) (Filing, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+filingCols+` FROM filings f JOIN messages m ON m.id = f.message_id WHERE f.message_id = $1`, messageID)
	if err != nil {
		return Filing{}, err
	}
	fs, err := scanFilings(rows)
	if err != nil {
		return Filing{}, err
	}
	if len(fs) == 0 {
		return Filing{}, ErrNotFound
	}
	return fs[0], nil
}

// MatterFilings lists a matter's filings, newest first.
func (s *Store) MatterFilings(ctx context.Context, matterID string, limit int) ([]Filing, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+filingCols+` FROM filings f JOIN messages m ON m.id = f.message_id WHERE f.matter_id = $1
ORDER BY f.filed_at DESC, f.message_id DESC LIMIT `+itoa(limit), matterID)
	if err != nil {
		return nil, err
	}
	return scanFilings(rows)
}

// FilingsOf lists the filings of messages.
func (s *Store) FilingsOf(ctx context.Context, messageIDs []string) (map[string]Filing, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+filingCols+` FROM filings f JOIN messages m ON m.id = f.message_id WHERE f.message_id = ANY($1)`, messageIDs)
	if err != nil {
		return nil, err
	}
	fs, err := scanFilings(rows)
	out := map[string]Filing{}
	for _, f := range fs {
		out[f.MessageID] = f
	}
	return out, err
}

// Unreported lists filings no summary has reported, oldest first.
func (s *Store) Unreported(ctx context.Context) ([]Filing, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+filingCols+` FROM filings f JOIN messages m ON m.id = f.message_id WHERE f.summary_sent_at IS NULL ORDER BY f.filed_at`)
	if err != nil {
		return nil, err
	}
	return scanFilings(rows)
}

// RecordSummary stores a sent summary and stamps the filings it reported.
func (s *Store) RecordSummary(ctx context.Context, recipients string, messageIDs []string, unfiled int) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.ms()
	if _, err := tx.ExecContext(ctx, `INSERT INTO summaries (id, sent_at, recipients, filing_count, unfiled_count) VALUES ($1,$2,$3,$4,$5)`,
		ids.New(s.Now()), now, recipients, len(messageIDs), unfiled); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE filings SET summary_sent_at = $2 WHERE message_id = ANY($1)`, messageIDs, now); err != nil {
		return err
	}
	return tx.Commit()
}

// LastSummaryAt is when the last summary went, or 0.
func (s *Store) LastSummaryAt(ctx context.Context) (int64, error) {
	var t sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT max(sent_at) FROM summaries`).Scan(&t)
	return t.Int64, err
}

// Summaries counts summaries and their filing counts (tests and the status page).
func (s *Store) Summaries(ctx context.Context) ([]int, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT filing_count FROM summaries ORDER BY sent_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// QueueAction appends a queue action.
func (s *Store) QueueAction(ctx context.Context, messageID, action, reason, userID string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO queue_actions (id, message_id, action, reason, user_id, at) VALUES ($1,$2,$3,$4,$5,$6)`,
		ids.New(s.Now()), messageID, action, reason, nullStr(userID), s.ms())
	return err
}

// --- outbound ---

// Outbound is one send attempt.
type Outbound struct {
	ID             string
	AccountID      string
	Kind           string
	IdempotencyKey string
	To, Cc, Bcc    []string
	Subject        string
	BodyText       string
	BodyHTML       string
	BodySHA256     string
	InReplyTo      string
	MatterID       string
	CreatedBy      string
	SystemKind     string
	Status         string
	ProviderRef    string
	Error          string
	Attempts       int
}

// BeginOutbound inserts a queued row for key, or returns the row already there.
func (s *Store) BeginOutbound(ctx context.Context, o *Outbound) (existing bool, err error) {
	if o.ID == "" {
		o.ID = ids.New(s.Now())
	}
	res, err := s.DB.ExecContext(ctx, `INSERT INTO outbound (id, account_id, kind, idempotency_key, to_addrs, cc_addrs, bcc_addrs, subject, body_text, body_html,
body_sha256, in_reply_to, matter_id, created_by, system_kind, status, created_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,'queued',$16)
ON CONFLICT (idempotency_key) DO NOTHING`, o.ID, nullStr(o.AccountID), o.Kind, o.IdempotencyKey, nonNil(o.To), nonNil(o.Cc), nonNil(o.Bcc), o.Subject,
		o.BodyText, o.BodyHTML, o.BodySHA256, o.InReplyTo, nullStr(o.MatterID), nullStr(o.CreatedBy), o.SystemKind, s.ms())
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		o.Status = "queued"
		return false, nil
	}
	got, err := s.OutboundByKey(ctx, o.IdempotencyKey)
	if err != nil {
		return false, err
	}
	*o = got
	return true, nil
}

// OutboundByKey reads a send by idempotency key.
func (s *Store) OutboundByKey(ctx context.Context, key string) (Outbound, error) {
	var o Outbound
	var acct, matter, by sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT id, account_id, kind, idempotency_key, to_addrs, cc_addrs, bcc_addrs, subject, body_text, body_html, body_sha256,
in_reply_to, matter_id, created_by, system_kind, status, provider_ref, error, attempts FROM outbound WHERE idempotency_key = $1`, key).
		Scan(&o.ID, &acct, &o.Kind, &o.IdempotencyKey, arr(&o.To), arr(&o.Cc), arr(&o.Bcc), &o.Subject, &o.BodyText, &o.BodyHTML, &o.BodySHA256,
			&o.InReplyTo, &matter, &by, &o.SystemKind, &o.Status, &o.ProviderRef, &o.Error, &o.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	o.AccountID, o.MatterID, o.CreatedBy = acct.String, matter.String, by.String
	return o, err
}

// FinishOutbound records a send's outcome. clearBody empties the body columns (system
// mail, once sent).
func (s *Store) FinishOutbound(ctx context.Context, id, status, providerRef, errText string, clearBody bool) error {
	var sentAt any
	if status == "sent" || status == "recorded" {
		sentAt = s.ms()
	}
	q := `UPDATE outbound SET status=$2, provider_ref=$3, error=$4, attempts = attempts + 1, sent_at = COALESCE($5, sent_at)`
	if clearBody {
		q += `, body_text = '', body_html = ''`
	}
	_, err := s.DB.ExecContext(ctx, q+` WHERE id=$1`, id, status, providerRef, errText, sentAt)
	return err
}

// SetOutboundStatus changes only the status (recorded or record_failed after filing).
func (s *Store) SetOutboundStatus(ctx context.Context, id, status string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE outbound SET status = $2 WHERE id = $1`, id, status)
	return err
}

// AddOutboundFile records an attached matter file.
func (s *Store) AddOutboundFile(ctx context.Context, outboundID, fileID, versionID string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO outbound_files (outbound_id, file_id, version_id) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, outboundID, fileID, versionID)
	return err
}

// --- events ---

// RecordEvent stores an event id once; it reports whether the event is new.
func (s *Store) RecordEvent(ctx context.Context, id, action, recordID string) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO received_events (event_id, action, record_id, received_at) VALUES ($1,$2,$3,$4) ON CONFLICT (event_id) DO NOTHING`,
		id, action, recordID, s.ms())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// --- locks ---

// TryLock takes a session advisory lock on key on its own connection; release with the
// returned func. It reports false when another run holds the lock.
func (s *Store) TryLock(ctx context.Context, key string) (func(), bool, error) {
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var ok bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(hashtextextended('mod_email:' || $1, 0))`, key).Scan(&ok); err != nil {
		conn.Close()
		return nil, false, err
	}
	if !ok {
		conn.Close()
		return nil, false, nil
	}
	return func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtextextended('mod_email:' || $1, 0))`, key)
		conn.Close()
	}, true, nil
}
