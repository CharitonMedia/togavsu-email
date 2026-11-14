# Togavsu Email

A Togavsu module that connects the firm's existing mail (Gmail through the Gmail API,
Microsoft 365 and Outlook.com through Microsoft Graph, and any IMAP/SMTP server), reads
it, sends it and files it to matters. The spec is [docs/SPEC.md](docs/SPEC.md) (from the
core repo's `docs/modules/email/SPEC.md`), under section 7 of the initial modules PRD.
Listed free, by Chariton Media.

The module runs in its own container and reaches core records only through the core
API, with its module token, the viewer's context token, and the match tokens and filing
grants the core issues for background filing, so walls, roles and attorney-only rules
come from the core. It keeps its own rows in the `mod_email` schema through its own
database login and never stores raw mail: a message's bytes are fetched again from the
mailbox at filing time and checked against the stored SHA-256.

Mail credentials are never settings, files or columns. Admins (firm and court accounts)
and mailbox owners (personal accounts) enter them at runtime on the Mail accounts tab;
each IMAP password or OAuth token set goes straight to the core's module secret store
under `account/<account id>/<kind>`.

## What it does

- **Accounts:** personal, filing and court accounts, with an after-filing action each
  (label, move, delete on filing accounts only), pause, resume and remove. Gmail and
  Microsoft connect by OAuth with PKCE in a top-level window; IMAP takes host, ports,
  security and login.
- **Sync:** a polling loop per account, one run at a time under an advisory lock, with
  IMAP UID resume and UIDVALIDITY resets, Gmail history ids and Graph delta links.
  Reading never marks mail read. Trash, Junk, Drafts, Filed and Review-Later are never
  read.
- **Matching** (spec 8.3): plus-address token, thread, address, court case number with
  party corroboration, court caption names, and an optional model pick for court
  notices through the core's AI calls.
- **Filing:** an HTML copy with the body sanitized and links kept, each real attachment
  as its own file linked to the copy, one note, and the account's after-filing action
  once the note is written. Every step carries an idempotency key, so an interrupted
  filing resumes without duplicates.
- **Unfiled queue** with suggestions, file, defer, restore, dismiss with a reason, retry,
  and File without attachments when the mailbox no longer holds the message.
- **Compose:** quick email, reply, reply all and forward from a matter, with matter
  files attached and the sent copy filed back to the matter.
- **System mail:** the core's system mail through the account marked for it (CC-1).
- **Outlook add-in** (`outlook-addin/`) with device grants: suggest, file and plus
  addresses.
- **Search, Inbox and Unfiled widgets, the matter Mail tab and Recent mail panel, and the
  filing summary.**

Every action is a JSON endpoint under `/m/email/api/`, listed in
`/m/email/api/openapi.json`; the pages' forms post to the same endpoints.

## Layout

```
cmd/email/                 serve and package subcommands
email.go                   wiring: Config, New, Handler, Run, package files
internal/server/           pages, JSON API, queue, accounts, OAuth, hooks, add-in API
internal/syncer/           the polling loop, processing, re-match, notices, summary
internal/match/            the matching rules over the core's matchMail answer
internal/filing/           filing a message to a matter
internal/send/             MIME building, quick email, replies, system mail
internal/provider/         the adapter interface; gmail/, graph/, imapsmtp/, fake/
internal/smtpsend/         context-bound SMTP that requires TLS
internal/mailparse/        MIME parsing; internal/render/ the HTML sanitizer
internal/oauth/            PKCE, token exchange and refresh
internal/coreapi/          core API client
internal/store/            mod_email reads and writes
internal/packaging/        packaging and signing on the core's pkg/modulekit
internal/bootstrap/        the only environment reads
internal/kittest/          the pending manifest fields and throwaway test keys
coresuite/                 acceptance suite on the real core (pkg/modulekit/coretest)
internal/fakecore/         in-memory core for tests
internal/pgtest/           scratch Postgres schema for tests
internal/testmail/         synthetic messages, a test SMTP listener and test certs
outlook-addin/             the add-in manifest template and task pane
package/                   togavsu-module.yaml and migrations 0001 to 0005
```

## Build and test

```
gofmt -l .
go vet ./...
TOGAVSU_TEST_DATABASE_URL='postgres://postgres:pg@127.0.0.1:5432/postgres?sslmode=disable' go test -race -timeout 0 ./...
docker build -t togavsu-email .
```

The module requires the core (`github.com/CharitonMedia/togavsu`, pinned in go.mod) for
`pkg/modulekit` and `pkg/modulekit/coretest`; no replace directive or go.work is used.

The source passes the core's settings source check (`modulekit.CheckSource`, AC-030),
test-support packages included, and `internal/packaging` holds it to that. Only
`internal/bootstrap` reads the environment. Every fixed number (a spec rule, a protocol
or format limit, a UUID offset, a strconv base) is a named constant in its package's
`fixed.go`, spelled from `zero` and `one` as the core's matter-clock example does, and
HTTP codes are `http.Status*`. What a firm adjusts is a setting in the manifest, read
from the core with no default of the module's own.

The unit Postgres tests use one scratch `mod_email_<random>` schema per test in the
`togavsu_email` database. `coresuite/` composes the real core with coretest: it installs
the signed package (trusted key, install, approval on the Modules tab), serves the module
in process with the token, webhook secret and database login the core handed over, and
drives each acceptance criterion the pinned core supports. coretest makes and drops its
own databases and roles. A criterion that needs a core item the pinned core lacks is
written out and skipped with `needs CORE-NN`. Without `TOGAVSU_TEST_DATABASE_URL` the
database tests skip.

No test reaches a real mail server or the network: mail comes from the fake mailbox set,
the go-imap v2 in-memory server, a local SMTP listener and `httptest` stand-ins of the
Gmail and Graph APIs, all on 127.0.0.1, with keys, certificates and tokens made at
runtime. The acceptance tests at the repo root still run on the fake core.

## Packaging and signing

`email package -key <seed file> -out email.signed.json` builds the installable archive
with `modulekit.Archive` and signs it with `modulekit.Build`; the key file holds a base64
Ed25519 seed, and the command prints the public key to trust. The image digest in the
manifest stays a placeholder. The package is checked with the core's own install checks
first, so it signs only what the core would install.

The manifest declares fields the spec's core changes add (listed once, with their CORE
items, in `internal/kittest`). The pinned core refuses them, so `email package` refuses
to sign for now; the tests install the package with those fields taken out and assert
that the core refuses each of them, so they fail as soon as the core accepts one. The
`togavsu-module check package` CI step returns when module-core lands.

## License

Same as Togavsu: source-available terms modeled on the Business Source License 1.1. See
[LICENSE](LICENSE).
