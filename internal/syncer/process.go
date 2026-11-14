// Package syncer runs the mail loop: each account's sync under a Postgres advisory
// lock, fetch retries, matching and filing of new mail, the hourly and event-driven
// re-match of the unfiled queue, notifications and the filing summary.
package syncer

import (
	"context"
	"errors"
	"strings"

	"github.com/CharitonMedia/togavsu-email/internal/config"
	"github.com/CharitonMedia/togavsu-email/internal/coreapi"
	"github.com/CharitonMedia/togavsu-email/internal/filing"
	"github.com/CharitonMedia/togavsu-email/internal/match"
	"github.com/CharitonMedia/togavsu-email/internal/render"
	"github.com/CharitonMedia/togavsu-email/internal/store"
)

// Processor decides and files one stored message in the background.
type Processor struct {
	Store *store.Store
	Core  *coreapi.Client
	Filer *filing.Filer
	// AIDenied is set when the core refused ai:use (the scope was declined).
	AIDenied func(bool)
}

// Outcome is what processing did.
type Outcome struct {
	Status   string // filed, unfiled, inbox, duplicate, pending (paused)
	MatterID string
	How      string
	Reason   string
}

// Input builds the rules' input from a stored message.
func Input(m store.Message, a store.Account, domains []string, thread []string) match.Input {
	text := m.BodyText
	if strings.TrimSpace(text) == "" {
		text = render.Text(m.BodyHTML)
	}
	in := match.Input{Role: a.Role, PlusAddressing: a.PlusAddressing, AccountAddress: a.Address, FirmDomains: domains,
		To: m.ToAddrs, Cc: m.CcAddrs, Subject: m.Subject, Text: text, ThreadMatters: thread, AIDeclined: m.AIDeclined}
	if m.FromAddr != "" {
		in.From = []string{m.FromAddr}
	}
	if m.DeliveredTo != "" {
		in.DeliveredTo = []string{m.DeliveredTo}
	}
	return in
}

// Process runs the section 8.3 order and, for a decided match, the section 8.4 filing
// under a filing grant. A paused account matches and files nothing.
func (p *Processor) Process(ctx context.Context, m store.Message, a store.Account, st config.Settings, domains []string) (Outcome, error) {
	// 1. Duplicate: the same Message-ID already filed to a matter.
	if matter, err := p.Store.FiledMatter(ctx, m.RFCMessageID); err != nil {
		return Outcome{}, err
	} else if matter != "" {
		if _, err := p.Store.Filing(ctx, m.ID); err == nil {
			return Outcome{Status: "filed", MatterID: matter}, nil
		}
		if err := p.Store.SetMessageStatus(ctx, m.ID, "duplicate", "duplicate", ""); err != nil {
			return Outcome{}, err
		}
		return Outcome{Status: "duplicate", MatterID: matter}, nil
	}
	refs := append([]string{}, m.ReferenceIDs...)
	if m.InReplyTo != "" {
		refs = append(refs, m.InReplyTo)
	}
	thread, err := p.Store.ThreadMatters(ctx, refs)
	if err != nil {
		return Outcome{}, err
	}
	in := Input(m, a, domains, thread)
	cfg := match.Config{Patterns: st.Patterns, AIPick: st.CourtAIPick}
	ans, err := p.Core.MatchMail(ctx, coreapi.Service, match.Request(in, cfg))
	if err != nil {
		return Outcome{}, err
	}
	pick := func(ctx context.Context, input map[string]any) (string, error) {
		var out struct {
			MatterID string `json:"matter_id"`
		}
		err := p.Core.AICall(ctx, coreapi.Task(ans.MatchToken), "court_notice_pick", input, &out)
		if p.AIDenied != nil {
			p.AIDenied(coreapi.IsScopeDenied(err))
		}
		return out.MatterID, err
	}
	d, err := match.Decide(ctx, in, cfg, ans, pick)
	if err != nil {
		return Outcome{}, err
	}
	if len(d.Contacts) > 0 {
		byContact := map[string]string{}
		for addr, c := range d.Contacts {
			byContact[c] = addr
		}
		if err := p.Store.SetContacts(ctx, m.ID, byContact); err != nil {
			return Outcome{}, err
		}
	}
	if d.AIDeclined {
		if err := p.Store.SetAIDeclined(ctx, m.ID); err != nil {
			return Outcome{}, err
		}
	}
	var ms []store.Match
	for _, c := range d.Candidates {
		ms = append(ms, store.Match{MatterID: c.MatterID, Via: c.Via, Rank: c.Rank})
	}
	if err := p.Store.SetMatches(ctx, m.ID, ms); err != nil {
		return Outcome{}, err
	}
	if a.Status == "paused" {
		reason := "paused"
		if d.Outcome == match.File {
			reason = "paused: would file (" + d.How + ")"
		}
		return Outcome{Status: "pending", Reason: reason}, p.Store.SetMessageStatus(ctx, m.ID, "pending", "paused", "")
	}
	switch d.Outcome {
	case match.Inbox:
		return Outcome{Status: "inbox", Reason: d.Reason}, p.Store.SetMessageStatus(ctx, m.ID, "inbox", d.Reason, "")
	case match.Queue:
		return Outcome{Status: "unfiled", Reason: d.Reason}, p.Store.SetMessageStatus(ctx, m.ID, "unfiled", d.Reason, "")
	}
	grant, err := p.Core.FilingGrant(ctx, ans.MatchToken, d.MatterID, m.ID)
	if err != nil {
		_ = p.Store.SetMessageStatus(ctx, m.ID, "unfiled", "filing_not_authorized", err.Error())
		return Outcome{Status: "unfiled", Reason: "filing_not_authorized"}, nil
	}
	_, err = p.Filer.File(ctx, filing.Request{Message: m, Account: a, MatterID: d.MatterID, How: d.How, Auth: coreapi.Task(grant)})
	switch {
	case err == nil:
		return Outcome{Status: "filed", MatterID: d.MatterID, How: d.How}, nil
	case errors.Is(err, filing.ErrDuplicate):
		return Outcome{Status: "duplicate", MatterID: d.MatterID}, nil
	case errors.Is(err, filing.ErrNotInMailbox):
		return Outcome{Status: "unfiled", Reason: filing.NotInMailbox}, nil
	}
	_ = p.Store.SetMessageStatus(ctx, m.ID, "unfiled", "filing_failed", err.Error())
	return Outcome{Status: "unfiled", Reason: "filing_failed"}, nil
}
