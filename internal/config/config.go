// Package config reads the module's settings (package/togavsu-module.yaml, settings)
// from the core's getModuleSettings answer.
package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/match"
)

// Settings are the admin's choices. The core answers every setting with its value or
// the manifest default, so nothing here has a default of its own.
type Settings struct {
	GoogleClientID        string
	GoogleClientSecret    string
	MicrosoftClientID     string
	MicrosoftClientSecret string
	MicrosoftTenant       string
	FirmDomains           []string // as set; empty means the connected accounts' domains
	Poll                  time.Duration
	Backfill              time.Duration
	Patterns              []*regexp.Regexp
	CourtAIPick           bool
	SummaryRecipients     []string
	SummaryInterval       time.Duration
	SendingDisabled       bool
}

// Parse reads the settings map. A numeric or yes/no setting that is missing or out of
// range is an error: the core always sends the value or the manifest default.
func Parse(m map[string]string) (Settings, error) {
	p := parser{m: m}
	s := Settings{
		GoogleClientID:        strings.TrimSpace(m["google_client_id"]),
		GoogleClientSecret:    m["google_client_secret"],
		MicrosoftClientID:     strings.TrimSpace(m["microsoft_client_id"]),
		MicrosoftClientSecret: m["microsoft_client_secret"],
		MicrosoftTenant:       p.required("microsoft_tenant"),
		FirmDomains:           List(m["firm_domains"]),
		Poll:                  time.Duration(p.num("poll_minutes", one)) * minute,
		Backfill:              time.Duration(p.num("backfill_days", zero)) * day,
		Patterns:              match.ParsePatterns(m["case_number_patterns"]),
		CourtAIPick:           p.boolean("court_ai_pick"),
		SummaryRecipients:     List(m["summary_recipients"]),
		SummaryInterval:       time.Duration(p.num("summary_interval_minutes", one)) * minute,
		SendingDisabled:       p.boolean("sending_disabled"),
	}
	return s, p.err
}

type parser struct {
	m   map[string]string
	err error
}

func (p *parser) fail(key string) {
	if p.err == nil {
		p.err = fmt.Errorf("config: setting %s is missing or invalid: %q", key, p.m[key])
	}
}

func (p *parser) required(key string) string {
	v := strings.TrimSpace(p.m[key])
	if v == "" {
		p.fail(key)
	}
	return v
}

func (p *parser) num(key string, min int) int {
	n, err := strconv.Atoi(strings.TrimSpace(p.m[key]))
	if err != nil || n < min {
		p.fail(key)
	}
	return n
}

func (p *parser) boolean(key string) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(p.m[key]))
	if err != nil {
		p.fail(key)
	}
	return b
}

// List splits a comma-separated setting, lower-cased.
func List(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Domains is the firm's mail domains: the setting, or the connected accounts' own.
func (s Settings) Domains(accountAddresses []string) []string {
	if len(s.FirmDomains) > 0 {
		return s.FirmDomains
	}
	seen := map[string]bool{}
	var out []string
	for _, a := range accountAddresses {
		if i := strings.LastIndexByte(a, '@'); i >= zero {
			d := strings.ToLower(a[i+1:])
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}
