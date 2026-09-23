# mailctl — plan

A single static Go binary that lets agents fetch and send mail from:
- **IMAP/SMTP** mailboxes (username + password or app password)
- **Microsoft 365 shared mailboxes** through Microsoft Graph, app-only (tenant is M365 Business Basic)

Agents call it as a CLI. It prints JSON on stdout. On failure it prints `{"error": "..."}` on stderr and exits 1.

## Commands

| Command | IMAP/SMTP | Graph |
|---|---|---|
| `list [--folder INBOX] [--unread] [--since DATE] [--limit N]` | UID SEARCH + FETCH envelope | `GET /users/{box}/mailFolders/{f}/messages` + `$filter`/`$top` |
| `get <id> [--save-attachments DIR]` | FETCH BODY[] + MIME parse | `GET …/messages/{id}` + `/attachments` |
| `send --to --subject [--cc] [--body-file F \| stdin] [--html] [--attach F]… [--reply-to <id>]` | Build MIME, send over SMTP, APPEND to Sent; set In-Reply-To/References on reply | `POST /users/{box}/sendMail`, or `…/messages/{id}/reply` |
| `mark <id> --read \| --unread` | STORE \Seen | `PATCH isRead` |
| `move <id> --to FOLDER` | UID MOVE | `POST …/messages/{id}/move` |
| `accounts` | list configured accounts: `[{name, type, address, description}]` | same |

Every command takes `--account NAME`. Any number of IMAP and Graph accounts can be configured; several shared mailboxes can use the same app registration (one config entry each, differing only in `mailbox`). The agent runs `accounts` once to find out what each mailbox is for, then passes the name on every call.

- IDs are opaque strings: the UID for IMAP (UIDVALIDITY is checked), the message ID for Graph.
- `list` returns `[{id, from, to, subject, date, snippet, unread, has_attachments}]`.
- `get` returns `{headers, text, html, attachments:[{name, size, content_type, path?}]}`.
- `get` never marks a message as read. Agents call `mark`/`move` explicitly, so the same mail isn't handled twice.

## Config

`~/.config/mailctl/config.toml` (override with `--config` or `MAILCTL_CONFIG`):

```toml
[accounts.work]
type = "imap"
description = "personal mail"
imap = "imap.example.com:993"      # implicit TLS
smtp = "smtp.example.com:587"      # STARTTLS (465 = implicit TLS)
user = "me@example.com"
password_cmd = "pass show mail/work"
sent_folder = "Sent"

[accounts.support]
type = "graph"
description = "customer support, reply in German"
tenant = "<tenant-id>"
client_id = "<client-id>"
client_secret_cmd = "pass show mail/graph"   # or cert_file = "..."
mailbox = "support@example.com"
```

Secrets never go in the file itself. They come from `*_cmd` or the env vars `MAILCTL_<ACCOUNT>_PASSWORD` / `_CLIENT_SECRET`.

## Graph setup (one time, M365 Business Basic)

Everything below is included in Business Basic (Entra ID Free + Exchange Online Plan 1). You need an admin account: Global Admin, or Application Admin + Exchange Admin. After this setup nobody has to log in. The only expiry is the client secret (max 24 months), or the certificate if you use one.

1. **Entra admin center → App registrations → New registration** (single tenant). Note the Tenant ID, the Client ID, and the Object ID of the **Enterprise Application**.
2. **Certificates & secrets:** create a client secret, or upload a certificate (preferred).
3. **Do not** add Mail.* application permissions in Entra, because they would cover every mailbox in the tenant. Limit access in Exchange instead:
   ```powershell
   Connect-ExchangeOnline
   New-ServicePrincipal -AppId <clientId> -ObjectId <enterpriseAppObjectId> -DisplayName "mailctl"
   New-ManagementScope -Name "mailctl-boxes" -RecipientRestrictionFilter "CustomAttribute1 -eq 'mailctl'"
   Set-Mailbox support@example.com -CustomAttribute1 mailctl      # tag each shared box
   New-ManagementRoleAssignment -App <clientId> -Role "Application Mail.ReadWrite" -CustomResourceScope "mailctl-boxes"
   New-ManagementRoleAssignment -App <clientId> -Role "Application Mail.Send"      -CustomResourceScope "mailctl-boxes"
   Test-ServicePrincipalAuthorization -Identity <clientId> -Resource support@example.com
   ```
   Changes take 30 minutes to 2 hours to apply. To add a mailbox later, tag it with `Set-Mailbox`.

Token: client-credentials flow against `https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token` with scope `https://graph.microsoft.com/.default`. A new token is fetched on each run; nothing is cached.

## Dependencies

- `github.com/emersion/go-imap/v2`: IMAP
- `github.com/emersion/go-message`: MIME parsing and building
- `github.com/BurntSushi/toml`: config
- stdlib `net/smtp`, `net/http`, `crypto/tls`: SMTP and Graph (no Graph SDK)

Build: `CGO_ENABLED=0 go build -o mailctl .`

## Build order

1. Config loading and `accounts`
2. IMAP `list` / `get`
3. SMTP `send` + APPEND to Sent
4. Graph token + `list` / `get` / `send`
5. `--reply-to`, `mark`, `move` on both backends
6. Attachments (download on `get`, attach on `send`; Graph attachments over 3 MB need an upload session)
7. Tests:
   - a MIME build/parse round-trip unit test
   - one smoke test per backend, run only when `MAILCTL_TEST_*` env vars are set

## Out of scope for v1

- MCP server mode: add if an agent harness can't shell out
- Search and delete
- OAuth for IMAP (Gmail / M365 IMAP)
- Token caching: add if rate limits become a problem

## Open items

- Test IMAP account credentials (needed for step 2)
- Entra app registration + tagged shared mailbox (needed for step 4)
