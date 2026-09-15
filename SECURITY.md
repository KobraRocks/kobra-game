# Security policy

The launcher binds a local HTTP socket, brokers a browser and writes the player's
save files, so its threat model is unusual and worth stating before you spend time
on a report.

## Reporting a vulnerability

Email **security@kobra.rocks**. Please do not open a public issue for anything
exploitable.

Include:

- what the flaw lets an attacker do, and who the attacker is (a web page the
  player visits? another local process? a malicious update server? a crafted game
  package?);
- the smallest reproduction you can manage — a command sequence, a modified
  fixture, or a test against `testgame/`;
- the launcher version (`launcher/launcher --version`), the release id, and the
  OS;
- whether you have told anyone else.

We aim to acknowledge within **3 business days** and to give an initial
assessment — in scope or not, and roughly how severe — within **10**. If a fix is
warranted we will agree a disclosure date with you; if we disagree about scope we
will say so plainly and explain why, rather than going quiet.

Good-faith research is welcome. Test against your own build and your own machine:
do not attack other people's installations, and do not run traffic interception or
load tests against infrastructure you do not own.

## Supported versions

The project is pre-1.0 and only `main` is supported. There are no maintained
release branches; a fix lands on `main`.

## The threat model, in one page

What the launcher defends against, and what it deliberately does not.

### In scope — we want reports on these

- **A hostile web page** the player visits while a game is running. Defences: the
  socket is loopback-only; `Host` must match the port exactly (anti-DNS-rebinding);
  a present `Origin` must be ours; writes need a session plus a double-submit CSRF
  token; responses carry CSP and `nosniff`; there is no CORS.
- **Reading or writing files outside the game's own folders.** Static serving is
  confined to the game tree, symlinks are resolved and re-checked, dot segments
  are rejected outright, and Windows device names are refused. Save identifiers
  are validated before a path is built.
- **A malicious or compromised update server.** Archives must be HTTPS unless they
  are loopback; the manifest's `archive.hash` and `archive.size` and every
  `files[].hash` are checked before anything is written; the archive must not
  contain the release manifest or touch `data/`; a declared signature fails
  closed. A manifest that declares no archive hash is refused rather than
  silently downgraded.
- **A crafted game package.** `kobra-pack check` and the launcher's extractor
  reject path traversal, absolute paths, symlinks, devices, hardlinks, reserved
  names, oversized expansions and entries that leave their prefix.
- **Information leaking through error messages.** Envelopes and diagnostics
  payloads are built so they cannot contain a filesystem path, an OS username, a
  token or a stack trace, and a test walks every error constructor to prove it.
- **Another local user on a shared machine.** Per-user state directories, an
  instance lock, and a private-file download mode.

### Out of scope — expected behaviour, not a vulnerability

- **Anything that requires write access to the game folder or to the player's
  `data/` folder.** That is the player's own machine and their own files; the
  launcher is not a sandbox against its own user.
- **A page that already has a session learning what game is running.** The probe
  and health endpoints are unauthenticated by design and disclose app, game id,
  instance, release and port. They carry no paths and no user data.
- **Static game files being readable over loopback.** `game/` is served without a
  session: it is the player's own content, already on their disk. Do not put
  secrets in a game package.
- **The game payload being untrusted by the launcher.** A game is code the player
  chose to run; it is not confined from itself.
- **Denial of service against your own launcher by your own machine** — filling the
  disk, exhausting ports, or killing the process.
- **Missing hardening features that are recorded as known gaps** in
  `CODE-REVIEW.md` (for example: the platform-specific paths are
  compile-verified but never executed on Windows or macOS, and nothing is code
  signed yet). A report that a Windows build is untested is useful feedback, but
  it is a known limitation rather than a vulnerability.

### Not security-relevant, but we still want to hear it

A crash, a hung launcher, a corrupted save, an update that will not apply, a
package the launcher refuses to run — those are bugs. Open a normal issue with
the templates in `.github/ISSUE_TEMPLATE/`.

## What we already guarantee

- **No telemetry and no background network access.** The only outbound request is
  an update check or download the player triggered against the URL in the game's
  own configuration.
- **`data/` is never modified by an update**, and is verified unchanged around the
  apply.
- **No recovery path deletes the player's data.** Damaged files and interrupted
  writes are quarantined into `data/.trash-<timestamp>/` and kept for the
  configured retention window.
- **Reproducible packages**, so a digest in a release manifest can be checked by
  anyone.

## Handling a report

If you are a maintainer receiving one:

1. Reproduce it against `testgame/` first; that is the fastest way to tell a
   launcher flaw from a game-package flaw.
2. Decide the class: a broken gate, a missing validation, an information leak, or
   a known gap. If it is a known gap, say so with a link, and still record it.
3. Fix the class, not the instance, and add a regression test named after the
   failure.
4. Update `CODE-REVIEW.md` if the report changes what is known about the threat
   model.
5. Credit the reporter in the fix commit unless they ask otherwise.
