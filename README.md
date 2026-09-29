# mailctl

A single static Go binary that lets agents fetch and send mail. CLI in, JSON out on stdout; on failure `{"error": "..."}` on stderr, exit 1.

Supports:

- **IMAP/SMTP** mailboxes (username + password, fetched via `*_cmd` or env var)
- **Microsoft 365 shared mailboxes** via Microsoft Graph, app-only auth (works with M365 Business Basic)

## Commands

```
mailctl login [--account NAME]                       # browser sign-in, config + token cache written
mailctl accounts                                   # list configured accounts
mailctl list [--folder F] [--unread] [--since DATE] [--limit N]
mailctl get <id> [--save-attachments DIR]          # never marks as read
mailctl send --to A [--cc A] --subject S [--body-file F | stdin] [--html] [--attach F]... [--reply-to <id>]
mailctl draft --to A [--cc A] --subject S [--body-file F | stdin] [--html] [--attach F]... [--reply-to <id>]   # files in drafts, returns {"id": ...}; Graph accounts only
mailctl mark <id> --read|--unread
mailctl move <id> --to FOLDER
```

Every command takes `--account NAME` (or `--config PATH`); omit it when only one account is configured. IDs are opaque strings (IMAP UID, Graph message ID).

## Config

`~/.config/mailctl/config.toml` (override: `--config` or `MAILCTL_CONFIG`):

```toml
[accounts.work]
type = "imap"
imap = "imap.example.com:993"
smtp = "smtp.example.com:587"
user = "me@example.com"
password_cmd = "pass show mail/work"

[accounts.support]
type = "graph"
tenant = "<tenant-id>"
client_id = "<client-id>"
client_secret_cmd = "pass show mail/graph"
mailbox = "support@example.com"
```

A graph account can also reuse a delegated sign-in's token cache (mailbox-cli
format, `graph-*.token.json`): set `token_file` instead of a client secret.
Both tools share and refresh the same file.

```toml
[accounts.work]
type = "graph"
tenant = "example.com"
client_id = "<client-id>"
mailbox = "me@example.com"
token_file = "/home/me/.config/mailbox/graph-work.token.json"
```

Secrets never go in the file: use `password_cmd`/`client_secret_cmd` or the env vars `MAILCTL_<ACCOUNT>_PASSWORD` / `_CLIENT_SECRET`.

## Graph setup (one time, M365 Business Basic)

For your own mailbox you don't need any of this — just run `mailctl login`.
It signs you in with the browser (PKCE, localhost redirect — the device-code
flow is blocked by most tenants' security defaults), saves the token cache in
mailbox-cli's format, and appends a graph account to config.toml.

A plain build has no built-in tenant: pass `--tenant` and `--client-id`, or
bake your own defaults in at build time (see Build below). Flags
`--tenant`/`--client-id` override the built-ins for other tenants.

The app-only setup below is for **shared mailboxes** (support@, info@), which
nobody logs into interactively:

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

## Build

A plain build works for IMAP and app-only Graph; `mailctl login` then needs
`--tenant`/`--client-id`. To make `mailctl login` work without flags, bake in
your own Entra tenant and app id — keep the real values out of the repo in an
untracked `defaults.env` (see `.gitignore`):

```
# defaults.env (untracked)
DEFAULT_TENANT=your-domain.example
DEFAULT_CLIENT_ID=00000000-0000-0000-0000-000000000000
```

```
set -a; . ./defaults.env; set +a
CGO_ENABLED=0 go build -ldflags="-s -w \
  -X main.defaultTenant=$DEFAULT_TENANT \
  -X main.defaultClientID=$DEFAULT_CLIENT_ID" -o mailctl .
```
