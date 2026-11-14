package match

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/CharitonMedia/togavsu-email/internal/coreapi"
)

var firm = []string{"firm.example.test"}

func pat() coreapi.MatchClient {
	return coreapi.MatchClient{ContactID: "c-pat", GivenName: "Pat", FamilyName: "Doe"}
}

func matter(id, number, status, cn string, via ...string) coreapi.MatchMatter {
	return coreapi.MatchMatter{ID: id, Number: number, Name: "Doe v. Roe", Status: status, CaseNumber: cn, Clients: []coreapi.MatchClient{pat()}, Via: via}
}

func contacts(addr string, ids ...string) map[string][]coreapi.MatchContact {
	out := map[string][]coreapi.MatchContact{addr: {}}
	for _, id := range ids {
		out[addr] = append(out[addr], coreapi.MatchContact{ContactID: id, GivenName: "Pat", FamilyName: "Doe"})
	}
	return out
}

func cfg() Config { return Config{Patterns: ParsePatterns(""), AIPick: true} }

func TestAddressRules(t *testing.T) {
	in := Input{Role: "filing", FirmDomains: firm, From: []string{"pat@client.example.test"}, To: []string{"filing@firm.example.test"}}
	cases := []struct {
		name    string
		in      Input
		ans     coreapi.MatchAnswer
		outcome string
		how     string
		reason  string
	}{
		{"MOD-EML-11 one contact on one open matter", in, coreapi.MatchAnswer{ContactsByAddress: contacts("pat@client.example.test", "c-pat"),
			Matters: []coreapi.MatchMatter{matter("m1", "2026-0042", "open", "", "address")}}, File, HowAddress, ""},
		{"MOD-EML-12 two contacts hold the address", in, coreapi.MatchAnswer{ContactsByAddress: contacts("pat@client.example.test", "c-pat", "c-other"),
			Matters: []coreapi.MatchMatter{matter("m1", "2026-0042", "open", "", "address")}}, Queue, "", MultipleContacts},
		{"MOD-EML-12 one contact on two open matters", in, coreapi.MatchAnswer{ContactsByAddress: contacts("pat@client.example.test", "c-pat"),
			Matters: []coreapi.MatchMatter{matter("m1", "2026-0042", "open", "", "address"), matter("m2", "2026-0043", "on_hold", "", "address")}},
			Queue, "", MultipleOpenMatters},
		{"no contact", in, coreapi.MatchAnswer{ContactsByAddress: contacts("pat@client.example.test")}, Queue, "", NoContact},
		{"MOD-EML-16 client mail never files to a closed matter", in, coreapi.MatchAnswer{ContactsByAddress: contacts("pat@client.example.test", "c-pat"),
			Matters: []coreapi.MatchMatter{matter("m1", "2026-0042", "closed", "", "address")}}, Queue, "", NoOpenMatter},
		{"firm only", Input{Role: "filing", FirmDomains: firm, From: []string{"a@firm.example.test"}, To: []string{"b@firm.example.test"}},
			coreapi.MatchAnswer{}, Queue, "", NoExternalAddress},
		{"personal mail with no contact stays in the inbox", Input{Role: "personal", FirmDomains: firm, From: []string{"x@shop.example.test"}},
			coreapi.MatchAnswer{ContactsByAddress: contacts("x@shop.example.test")}, Inbox, "", NoContact},
		{"personal ambiguous goes to the queue", Input{Role: "personal", FirmDomains: firm, From: []string{"pat@client.example.test"}},
			coreapi.MatchAnswer{ContactsByAddress: contacts("pat@client.example.test", "c-pat"),
				Matters: []coreapi.MatchMatter{matter("m1", "1", "open", "", "address"), matter("m2", "2", "open", "", "address")}}, Queue, "", MultipleOpenMatters},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := Decide(context.Background(), c.in, cfg(), c.ans, nil)
			if err != nil {
				t.Fatal(err)
			}
			if d.Outcome != c.outcome || d.How != c.how || d.Reason != c.reason {
				t.Fatalf("got %+v", d)
			}
			if c.outcome == File && d.MatterID != "m1" {
				t.Fatalf("filed to %s", d.MatterID)
			}
		})
	}
}

func TestDirection(t *testing.T) {
	if Direction(Input{Role: "personal", FirmDomains: firm, From: []string{"me@firm.example.test"}}) != "outbound" {
		t.Fatal("firm From is outbound")
	}
	if Direction(Input{Role: "personal", FirmDomains: firm, From: []string{"pat@client.example.test"}}) != "inbound" {
		t.Fatal("client From is inbound")
	}
	if Direction(Input{Role: "court", From: []string{"me@firm.example.test"}, FirmDomains: firm}) != "court" {
		t.Fatal("court")
	}
}

func TestPlusToken(t *testing.T) {
	base := Input{Role: "filing", PlusAddressing: true, AccountAddress: "filing@firm.example.test", FirmDomains: firm,
		From: []string{"stranger@else.example.test"}, To: []string{"filing+2026-0042@firm.example.test"}}
	if PlusToken(base) != "2026-0042" {
		t.Fatalf("token %q", PlusToken(base))
	}
	r := Request(base, cfg())
	if !reflect.DeepEqual(r.MatterNumbers, []string{"2026-0042"}) {
		t.Fatalf("request %+v", r)
	}
	// MOD-EML-13: files by token though the sender is unknown, and any status counts.
	ans := coreapi.MatchAnswer{ContactsByAddress: contacts("stranger@else.example.test"),
		Matters: []coreapi.MatchMatter{matter("m1", "2026-0042", "closed", "", "matter_number")}}
	d, _ := Decide(context.Background(), base, cfg(), ans, nil)
	if d.Outcome != File || d.How != HowToken || d.MatterID != "m1" {
		t.Fatalf("token: %+v", d)
	}
	// A last-name token on two open matters goes to the queue.
	byName := base
	byName.To = []string{"filing+doe@firm.example.test"}
	ans = coreapi.MatchAnswer{Matters: []coreapi.MatchMatter{matter("m1", "1", "open", "", "client_name"), matter("m2", "2", "open", "", "client_name")}}
	d, _ = Decide(context.Background(), byName, cfg(), ans, nil)
	if d.Outcome != Queue || d.Reason != TokenMultiple || len(d.Candidates) != 2 {
		t.Fatalf("surname token: %+v", d)
	}
	// One open matter for the surname files; closed ones don't count.
	ans = coreapi.MatchAnswer{Matters: []coreapi.MatchMatter{matter("m1", "1", "open", "", "client_name"), matter("m2", "2", "closed", "", "client_name")}}
	d, _ = Decide(context.Background(), byName, cfg(), ans, nil)
	if d.Outcome != File || d.MatterID != "m1" {
		t.Fatalf("one open surname: %+v", d)
	}
	// Plus addressing off, or a personal account: no token.
	off := base
	off.PlusAddressing = false
	if PlusToken(off) != "" {
		t.Fatal("token with plus addressing off")
	}
}

func TestThread(t *testing.T) {
	// MOD-EML-18: a reply from an address with no contact files to the thread's matter.
	in := Input{Role: "filing", FirmDomains: firm, From: []string{"assistant@else.example.test"}, ThreadMatters: []string{"m1"}}
	ans := coreapi.MatchAnswer{ContactsByAddress: contacts("assistant@else.example.test"),
		Matters: []coreapi.MatchMatter{matter("m1", "2026-0042", "open", "", "known")}}
	d, _ := Decide(context.Background(), in, cfg(), ans, nil)
	if d.Outcome != File || d.How != HowThread || d.MatterID != "m1" {
		t.Fatalf("thread: %+v", d)
	}
	// A closed thread matter doesn't take the reply.
	ans.Matters[0].Status = "closed"
	d, _ = Decide(context.Background(), in, cfg(), ans, nil)
	if d.Outcome != Queue {
		t.Fatalf("closed thread: %+v", d)
	}
	// Not returned as known (walled from the service principal): no thread filing.
	ans.Matters = nil
	d, _ = Decide(context.Background(), in, cfg(), ans, nil)
	if d.Outcome != Queue {
		t.Fatalf("unknown thread: %+v", d)
	}
}

func TestDefaultPatterns(t *testing.T) {
	ps := ParsePatterns("")
	cases := map[string][]string{
		"Notice of filing in 26AB-CV00123 Doe v. Roe":        {"26AB-CV00123"},
		"Case 26AB-CV00123-01 Order entered":                 {"26AB-CV00123-01"},
		"Appeal ED112345 filed":                              {"ED112345"},
		"Trial 24SL-CC01234 and appeal WD87654 both listed":  {"24SL-CC01234"},
		"Supreme Court SC100123":                             {"SC100123"},
		"Nothing here but 2026-0042":                         nil,
		"Lower case 26ab-cv00123 is not a filed case number": nil,
	}
	for text, want := range cases {
		if got := CaseNumbers(text, ps); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %v want %v", text, got, want)
		}
	}
}

func notice(text string) Input {
	return Input{Role: "court", FirmDomains: firm, From: []string{"noreply@courts.example.test"}, Subject: "Notice of Electronic Filing", Text: text}
}

func TestCourtRules(t *testing.T) {
	ctx := context.Background()
	// MOD-EML-14: a notice for the suffixed number never files to the base number,
	// by field or by name.
	in := notice("Case 26AB-CV00123-01 Pat Doe v. Roe. An order was entered.")
	ans := coreapi.MatchAnswer{Matters: []coreapi.MatchMatter{
		matter("m1", "2026-0042", "open", "26AB-CV00123"),
		{ID: "m2", Number: "2026-0050", Name: "Doe 26AB-CV00123", Status: "open", Clients: []coreapi.MatchClient{pat()}, Via: []string{"case_number_in_name"}},
	}}
	d, _ := Decide(ctx, in, Config{Patterns: ParsePatterns("")}, ans, nil)
	if d.Outcome == File && d.How != HowCourtName {
		t.Fatalf("suffix filed by number: %+v", d)
	}
	for _, c := range d.Candidates {
		if c.Via == "case_number" || c.Via == "case_number_in_name" {
			t.Fatalf("suffix candidate by number: %+v", d)
		}
	}
	if !InName("Doe 26AB-CV00123-01 trial", "26AB-CV00123-01") || InName("Doe 26AB-CV00123-01", "26AB-CV00123") || InName("X26AB-CV00123", "26AB-CV00123") {
		t.Fatal("InName boundary")
	}

	// Exact number with the client's last name in the party text files.
	in = notice("Case 26AB-CV00123 Pat Doe, Plaintiff v. Roe. Order entered.")
	ans = coreapi.MatchAnswer{Matters: []coreapi.MatchMatter{matter("m1", "2026-0042", "open", "26AB-CV00123", "case_number")}}
	d, _ = Decide(ctx, in, cfg(), ans, nil)
	if d.Outcome != File || d.How != HowCourt || d.MatterID != "m1" {
		t.Fatalf("court: %+v", d)
	}
	// The number written in the name files too.
	ans = coreapi.MatchAnswer{Matters: []coreapi.MatchMatter{{ID: "m1", Name: "Doe - 26AB-CV00123", Status: "open", Clients: []coreapi.MatchClient{pat()}, Via: []string{"case_number_in_name"}}}}
	d, _ = Decide(ctx, in, cfg(), ans, nil)
	if d.Outcome != File || d.How != HowCourt {
		t.Fatalf("court by name: %+v", d)
	}

	// MOD-EML-15: the number matches but the party text lacks the client's last name.
	in = notice("Case 26AB-CV00123 Smith v. Jones. Order entered.")
	ans = coreapi.MatchAnswer{Matters: []coreapi.MatchMatter{matter("m1", "2026-0042", "open", "26AB-CV00123", "case_number")}}
	d, _ = Decide(ctx, in, cfg(), ans, nil)
	if d.Outcome != Queue || d.Reason != PartyMismatch || len(d.Candidates) != 1 {
		t.Fatalf("mismatch: %+v", d)
	}

	// MOD-EML-16: court mail matches a closed matter by exact case number.
	in = notice("Case 26AB-CV00123 Pat Doe v. Roe. Judgment.")
	ans = coreapi.MatchAnswer{Matters: []coreapi.MatchMatter{matter("m1", "2026-0042", "closed", "26AB-CV00123", "case_number")}}
	d, _ = Decide(ctx, in, cfg(), ans, nil)
	if d.Outcome != File || d.MatterID != "m1" {
		t.Fatalf("closed court: %+v", d)
	}

	// No number: the client's first and last name in the caption, one matter.
	in = notice("Filed on behalf of: Pat Doe. Hearing set.")
	ans = coreapi.MatchAnswer{Matters: []coreapi.MatchMatter{matter("m1", "2026-0042", "open", "", "client_name")}}
	d, _ = Decide(ctx, in, Config{Patterns: ParsePatterns("")}, ans, nil)
	if d.Outcome != File || d.How != HowCourtName {
		t.Fatalf("court name: %+v", d)
	}
	if r := Request(in, cfg()); !contains(r.NameTokens, "doe") || len(r.Addresses) != 0 {
		t.Fatalf("court request %+v", r)
	}
	// No number and nothing else.
	d, _ = Decide(ctx, notice("hello there"), cfg(), coreapi.MatchAnswer{}, nil)
	if d.Outcome != Queue || d.Reason != NoCaseNumber {
		t.Fatalf("nothing: %+v", d)
	}
}

func TestCourtAIPick(t *testing.T) {
	ctx := context.Background()
	// Surname only (no first name) on two matters, so the deterministic steps leave candidates.
	in := notice("Notice to Doe regarding hearing.")
	m1, m2 := matter("m1", "2026-0042", "open", "", "client_name"), matter("m2", "2026-0043", "open", "", "client_name")
	m2.Clients = []coreapi.MatchClient{{ContactID: "c-sam", GivenName: "Sam", FamilyName: "Doe"}}
	ans := coreapi.MatchAnswer{Matters: []coreapi.MatchMatter{m1, m2}}
	ask := func(answer string, err error) (Picker, *int) {
		n := 0
		return func(context.Context, map[string]any) (string, error) { n++; return answer, err }, &n
	}

	// MOD-EML-17: an answer naming a candidate files with auto_court_ai.
	p, n := ask("m2", nil)
	d, _ := Decide(ctx, in, cfg(), ans, p)
	if d.Outcome != File || d.How != HowCourtAI || d.MatterID != "m2" || *n != 1 {
		t.Fatalf("ai pick: %+v", d)
	}
	// An answer naming a matter outside the candidates files nothing.
	p, _ = ask("m-elsewhere", nil)
	d, _ = Decide(ctx, in, cfg(), ans, p)
	if d.Outcome != Queue || d.Reason != AINotCandidate {
		t.Fatalf("not candidate: %+v", d)
	}
	// "none" sets ai_declined, and the next match doesn't ask again.
	p, n = ask("", nil)
	d, _ = Decide(ctx, in, cfg(), ans, p)
	if d.Outcome != Queue || d.Reason != AINone || !d.AIDeclined {
		t.Fatalf("none: %+v", d)
	}
	in.AIDeclined = true
	d, _ = Decide(ctx, in, cfg(), ans, p)
	if *n != 1 || d.Outcome != Queue || d.AIAsked {
		t.Fatalf("asked again: %d %+v", *n, d)
	}
	// Turned off: never asked.
	in.AIDeclined = false
	p, n = ask("m1", nil)
	d, _ = Decide(ctx, in, Config{Patterns: ParsePatterns("")}, ans, p)
	if *n != 0 || d.Outcome != Queue {
		t.Fatalf("off: %+v", d)
	}
	// A failed call queues with its reason.
	p, _ = ask("", errors.New("down"))
	d, _ = Decide(ctx, in, cfg(), ans, p)
	if d.Outcome != Queue || d.Reason != AIFailed || d.AIDeclined {
		t.Fatalf("failed: %+v", d)
	}
}

func TestNameTokensCapped(t *testing.T) {
	text := ""
	for i := 0; i < 80; i++ {
		text += " Name" + string(rune('A'+i%26)) + string(rune('a'+i/26))
	}
	if n := len(NameTokens("", text)); n != 50 {
		t.Fatalf("got %d tokens", n)
	}
}
