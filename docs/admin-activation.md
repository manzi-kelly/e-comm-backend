# First-time administrator activation (backend contract)

This document is the backend-only integration contract for a future Next.js
admin activation screen. No frontend files were changed.

## Scope and invariants

- The only eligible address is the compiled-in constant
  `autivatechltd@gmail.com` (`config.AuthorizedAdminEmail`). It cannot be set
  through an environment variable or any HTTP endpoint.
- There is no unrestricted admin registration. The initial account cannot exist
  until all three activation steps succeed.
- The server never creates an administrator at startup.
- `migrate` only creates activation tables. An explicit CLI command provisions
  the authorized email; it creates no account and no password.
- The account created by `complete` is `super_admin`, active, with an Argon2id
  password hash. Existing administrator accounts are never modified and block
  activation if any exist.
- All state changes are serialized by a PostgreSQL transaction advisory lock;
  the authorization and code rows are locked `FOR UPDATE`. One-time behavior is
  additionally enforced by partial unique indexes on the authorization table.

## Explicit development/test setup

```powershell
$env:DATABASE_URL = 'postgres://<user>:<password>@127.0.0.1:5432/<dev-db>?sslmode=disable'
$env:AUTIVASHOP_ALLOW_ACTIVATION_SETUP = 'true'
# optional: $env:ADMIN_ACTIVATION_DISPLAY_NAME = 'AutivaShop Administrator'
go run ./cmd/api migrate
go run ./cmd/api provision-admin-activation
```

The setup command refuses production, refuses when any admin already exists,
is idempotent, and prints no email, code, password, or secret. It requires
migration `0007_admin_activation.sql` to be applied first.

## HTTP endpoints

All three endpoints require:

- `Content-Type: application/json`
- an allowed exact `Origin` (or allowed `Referer` origin)
- double-submit CSRF: the `autiva_admin_csrf` cookie value must equal the
  `X-CSRF-Token` header (configurable via `AUTH_CSRF_HEADER_NAME`)

The unauthenticated `GET /api/v1/admin/auth/me` response includes a bootstrap
`csrfToken` and sets the CSRF cookie. Use that token for the three POSTs.
Responses use the existing bare-object success format and
`{"error":{"code","message","requestId","fields"}}` error envelope.

### 1. Request an activation code

`POST /api/v1/admin/activation/request-code`

```json
{ "email": "autivatechltd@gmail.com" }
```

Success: `202 Accepted`

```json
{ "status": "accepted" }
```

This response is identical for authorized, unauthorized, and already-activated
addresses. Mail is sent only to the provisioned authorized address. Errors:
`429 rate_limited`, `502 storage_unavailable` (real delivery failure; no code
is usable), `503 storage_unavailable` (delivery not configured), `403 forbidden`
(Origin/CSRF), `400 bad_request` (invalid JSON).

### 2. Verify the code

`POST /api/v1/admin/activation/verify-code`

```json
{ "email": "autivatechltd@gmail.com", "code": "123456" }
```

Success: `200 OK`

```json
{ "status": "verified", "activationToken": "<single-use, short-lived token>" }
```

Errors: `400 bad_request` with the generic message
`The activation code is invalid or has expired.` for wrong, reused, expired or
unknown codes; `429 rate_limited`; `503 storage_unavailable`; `403 forbidden`;
`400 bad_request` for invalid body shape. The code itself is never returned.

### 3. Complete activation (create password and account)

`POST /api/v1/admin/activation/complete`

```json
{
  "email": "autivatechltd@gmail.com",
  "activationToken": "<token from verify-code>",
  "password": "<at least 12 characters; existing admin password policy>"
}
```

Success: `201 Created`

```json
{
  "status": "activated",
  "admin": {
    "id": "<uuid>",
    "email": "autivatechltd@gmail.com",
    "displayName": "AutivaShop Administrator",
    "role": "super_admin",
    "isActive": true
  }
}
```

Errors: `422 validation_failed` with `fields.password` for a weak password,
`400 bad_request` for invalid/expired/replayed tokens, `429 rate_limited`,
`503 storage_unavailable`, `403 forbidden`. No session is issued; sign in
through the unchanged `POST /api/v1/admin/auth/login` afterwards.

## OTP and token properties

- Six numeric digits from `crypto/rand`; five-minute fixed expiry.
- Stored only as HMAC-SHA256(key, domain || normalized email || code).
- Verification uses constant-time comparison and consumes the code atomically.
- Wrong attempts are counted in durable state and in an IP/email limiter;
  exhausting them consumes the code.
- Resends are capped per window and per cooldown, with durable IP/email counts.
- A new code invalidates all older codes for the authorization.
- Verification issues a high-entropy opaque token, stored only as a SHA-256
  hash with a short expiry (default five minutes). Completion invalidates the
  token, all codes, and the pending status in one transaction.

## Required environment for real delivery

All values come from the process environment or a secret manager; none belong
in source control. Missing values disable activation endpoints with HTTP 503
and an explicit list of required variable names in the error message.

**SMTP password source:** for this local development environment the owner has
chosen `D:\backend\.env` as the authoritative source for Gmail SMTP settings,
including `ADMIN_ACTIVATION_SMTP_PASSWORD`. The Go application itself reads only
process environment variables. `Start-AutivaShopDevApi.ps1` loads the protected
`activation.env` first for `ADMIN_ACTIVATION_OTP_KEY`, then loads `D:\backend\.env`
as the authoritative SMTP configuration, re-asserts the OTP key, and fails closed
if the `.env` password is missing or empty. The launcher no longer reads Windows
Credential Manager; the pre-existing `AutivaShop/Dev/SMTP` entry is left
untouched. Production secret handling is unchanged. Do not paste the Gmail App
Password into shell history, source control, logs, or chat, and do not print it
in verification output.

| Variable | Purpose |
| --- | --- |
| `ADMIN_ACTIVATION_OTP_KEY` | Base64, at least 32 bytes of HMAC key material |
| `ADMIN_ACTIVATION_SMTP_HOST` | Approved SMTP relay host |
| `ADMIN_ACTIVATION_SMTP_PORT` | Relay port (default 587) |
| `ADMIN_ACTIVATION_SMTP_USERNAME` | SMTP user (optional for local relays) |
| `ADMIN_ACTIVATION_SMTP_PASSWORD` | SMTP credential (never hardcode) |
| `ADMIN_ACTIVATION_SMTP_FROM` | Verified sender address |
| `ADMIN_ACTIVATION_SMTP_TLS` | `starttls` (default), `tls`, or `none` in dev/test only |
| `ADMIN_ACTIVATION_SMTP_TIMEOUT` | SMTP timeout (default 10s) |
| `ADMIN_ACTIVATION_MAX_VERIFY_ATTEMPTS` | Failed code attempts (default 5) |
| `ADMIN_ACTIVATION_MAX_RESENDS` | Codes per resend window (default 3) |
| `ADMIN_ACTIVATION_RESEND_WINDOW` | Resend/throttle window (default 1h) |
| `ADMIN_ACTIVATION_REQUEST_COOLDOWN` | Minimum interval between codes (default 60s) |
| `ADMIN_ACTIVATION_AUTH_TTL` | Single-use token lifetime (1m..15m, default 5m) |
| `AUTIVASHOP_ALLOW_ACTIVATION_SETUP` | Enables the explicit CLI setup command only |

Automated tests inject a fake `EmailSender`; no real mail is sent and no SMTP
credential is needed for `go test`.
