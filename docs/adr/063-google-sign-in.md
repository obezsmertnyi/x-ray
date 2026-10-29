# ADR-063: Google Sign-In, Restricted to a Workspace Domain

**Status:** Implemented

## Context

Sign-in offers GitHub (ADR-022) and Microsoft Entra. A team on Google Workspace has
neither for everyone: people must create GitHub accounts or the organisation must run
an Entra tenant just to reach the UI, voice and MCP.

## Decision

Google is a third provider, built like Entra: `--google-client-id`,
`--google-client-secret` and `--google-domain` (or `GOOGLE_*`, chart
`google.secretName`), routes `/v1/auth/google` and `/v1/auth/google/callback`, and
`google:<sub>` as the user ID, since `sub` is stable and the email can change.

- **The domain is checked on the ID token's `hd` claim, not on the email.** Google
  sets `hd` only for Workspace accounts. A consumer Google account can be registered
  on any address, so `someone@example.com` proves nothing about `example.com`. The
  `hd=` parameter on the authorize URL only narrows the account picker.
- `email_verified`, `aud`, `iss` and `exp` are checked too. Every check fails closed:
  a claim of the wrong type is a refusal.
- The ID token comes straight from Google's token endpoint over TLS with the client
  secret, so its signature is not verified again (OpenID Connect Core 3.1.3.7).
- The startup rules of ADR-027 apply: a Google client without `--google-domain`
  refuses to start unless `--allow-any-user`, and `--dev` cannot be combined with it.
- The Google client secret joins the inputs of the session key and the MCP signing
  seed (ADR-047, ADR-050). Empty inputs are skipped, so a deployment without Google
  derives the same keys as before and nobody is logged out.

## Consequences

- A Workspace team signs in with the accounts it already has.
- The token exchange has a 10 s timeout, and the response is read up to 1 MiB.
- `NewServer` takes options (`WithGoogleOAuth`), so its existing callers are unchanged.
