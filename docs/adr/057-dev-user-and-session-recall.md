# ADR-057: Sign-In Off Runs as a Dev User; a Voice Session Opens With the User's Past

**Status:** Implemented

## Context

With no sign-in provider (`--dev`) there was no caller identity. Voice saved a
note under the key of an empty user ID, while the notes list and the live event
stream returned nothing for a caller without a session, so a note the model had
just written never showed in the UI. Voice search merged private notes by keyword
only. And a session began with no memory of earlier ones: the model had to guess
to call `recent_memory` or `search_memory`.

## Decision

1. **A built-in user.** With sign-in off, every request without a session is the
   user `dev`: the server holds one real session for it and attaches it when a
   request carries none. Voice, personal notes, the event stream and
   `/v1/auth/me` then see one stable identity. A session the caller does send
   keeps its own identity, and behind a provider nothing changes.
2. **Private notes are searched like everything else.** Voice `search_memory`
   merges the user's private notes through the ranked search (keywords, then
   meaning), and `list_memory` includes them for the `note` kind.
3. **The session opens with the past.** The system prompt gets the user's ten
   newest private notes and three newest session memories, one line each (control
   characters removed, 240 characters), labelled as data and not instructions.

## Consequences

- In `--dev` the per-user voice quota now applies to `dev`, where the anonymous
  caller was never metered.
- The prompt grows by up to about 13 short lines per session.
- A model can still end a session without saving a session memory. That is left to
  the model and the prompt; no server rule forces it.

## Tests

`TestServer_UserNotes_AuthDisabled_ShowsTheDevUsersNotes`, `TestServer_DevUser_OnlyWithSignInOff`,
`TestVoiceManager_UserRecall`, `TestListMemory_IncludesTheCallersPrivateNotes`.
