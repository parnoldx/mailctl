# mailctl

A single static Go binary that lets agents fetch and send mail. CLI in, JSON out on stdout; on failure `{"error": "..."}` on stderr, exit 1.

Supports:

- **IMAP/SMTP** mailboxes (username + password, fetched via `*_cmd` or env var)
- **Microsoft 365 shared mailboxes** via Microsoft Graph, app-only auth (works with M365 Business Basic)

## Commands

```
mailctl accounts                                   # list configured accounts
mailctl list [--folder F] [--unread] [--since DATE] [--limit N]
mailctl get <id> [--save-attachments DIR]          # never marks as read
mailctl send --to A [--cc A] --subject S [--body-file F | stdin] [--html] [--attach F]... [--reply-to <id>]
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

Secrets never go in the file: use `password_cmd`/`client_secret_cmd` or the env vars `MAILCTL_<ACCOUNT>_PASSWORD` / `_CLIENT_SECRET`.

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

## Build

```
CGO_ENABLED=0 go build -ldflags="-s -w" -o mailctl .
```
