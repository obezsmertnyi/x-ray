# Security Policy

## Reporting a vulnerability

Please do not open a public issue for security problems. Use GitHub's
[private vulnerability reporting](https://github.com/triageagent-dev/x-ray/security/advisories/new)
for this repository.

Include what you found, how to reproduce it, and the version or commit you tested.
You can expect an acknowledgement within a few days.

## Supported versions

Only the latest release and `main` receive fixes.

## Scope

X-Ray handles OAuth sessions, MCP bearer tokens and API keys for the model
providers it calls. Reports about authentication, session handling, CORS/origin
checks, token storage and tenant isolation are especially welcome. See
`docs/adr/027-public-auth-hardening.md` and `docs/adr/032-public-exposure-hardening.md`
for the current hardening decisions.
