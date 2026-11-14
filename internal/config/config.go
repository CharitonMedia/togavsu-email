// Package config reads the module's settings (package/togavsu-module.yaml, settings)
// from the core's getModuleSettings answer.
package config

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/match"
)

// Settings are the admin's choices, with the manifest defaults.
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

// Parse reads the settings map.
func Parse(m map[string]string) Settings {
	s := Settings{
		GoogleClientID:        strings.TrimSpace(m["google_client_id"]),
		GoogleClientSecret:    m["google_client_secret"],
		MicrosoftClientID:     strings.TrimSpace(m["microsoft_client_id"]),
		MicrosoftClientSecret: m["microsoft_client_secret"],
		MicrosoftTenant:       strings.TrimSpace(m["microsoft_tenant"]),
		FirmDomains:           List(m["firm_domains"]),
		Poll:                  time.Duration(num(m["poll_minutes"], 5, 1)) * time.Minute,
		Backfill:              time.Duration(num(m["backfill_days"], 30, 0)) * 24 * time.Hour,
		Patterns:              match.ParsePatterns(m["case_number_patterns"]),
		CourtAIPick:           boolean(m["court_ai_pick"], true),
		SummaryRecipients:     List(m["summary_recipients"]),
		SummaryInterval:       time.Duration(num(m["summary_interval_minutes"], 60, 1)) * time.Minute,
		SendingDisabled:       boolean(m["sending_disabled"], false),
	}
	if s.MicrosoftTenant == "" {
		s.MicrosoftTenant = "organizations"
	}
	return s
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

func num(v string, def, min int) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < min {
		return def
	}
	return n
}

func boolean(v string, def bool) bool {
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return b
}

// Domains is the firm's mail domains: the setting, or the connected accounts' own.
func (s Settings) Domains(accountAddresses []string) []string {
	if len(s.FirmDomains) > 0 {
		return s.FirmDomains
	}
	seen := map[string]bool{}
	var out []string
	for _, a := range accountAddresses {
		if i := strings.LastIndexByte(a, '@'); i >= 0 {
			d := strings.ToLower(a[i+1:])
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}
