package config_test

import (
	"regexp"
	"testing"
	"time"

	"github.com/CharitonMedia/togavsu-email/internal/config"
	"github.com/CharitonMedia/togavsu-email/internal/ids"
	pkgfiles "github.com/CharitonMedia/togavsu-email/package"
)

// The manifest defaults, as the core sends them for unset settings, parse to the spec's
// values; a numeric or yes/no setting that is missing or out of range is an error, not a
// default of the module's own.
func TestParseTakesEveryValueFromTheCore(t *testing.T) {
	d, err := pkgfiles.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	s, err := config.Parse(d)
	if err != nil {
		t.Fatal(err)
	}
	if s.Poll != 5*time.Minute || s.Backfill != 30*24*time.Hour || s.SummaryInterval != time.Hour ||
		!s.CourtAIPick || s.SendingDisabled || s.MicrosoftTenant != "organizations" || len(s.Patterns) != 2 {
		t.Fatalf("%+v", s)
	}
	for _, key := range []string{"poll_minutes", "backfill_days", "summary_interval_minutes", "court_ai_pick", "sending_disabled", "microsoft_tenant"} {
		m := map[string]string{}
		for k, v := range d {
			m[k] = v
		}
		delete(m, key)
		if _, err := config.Parse(m); err == nil {
			t.Errorf("%s missing: no error", key)
		}
	}
	d["poll_minutes"] = "0"
	if _, err := config.Parse(d); err == nil {
		t.Error("poll_minutes 0 accepted")
	}
}

// Row ids are UUIDv7 text.
func TestIDsAreUUIDv7(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	at := time.UnixMilli(0x0123456789ab)
	id := ids.New(at)
	if !re.MatchString(id) || id[:13] != "01234567-89ab" {
		t.Fatalf("%s", id)
	}
}
