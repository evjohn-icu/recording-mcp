# Local memory v2 execution plan

Baseline: `59eb934370a46c4ff41c518a8b2a19dbe285ffb5`. Existing worktree was clean.

The accepted scope is local independent password-protected profiles, transcript
revision correction and speaker review, local forgetting with replay fences,
fact valid-time/recorded-time history, and optional pure-Go ANN sidecars.
SQLite remains authoritative; no CGO, driver replacement or cloud deletion.

## Work ownership

| Package | Owner | Boundary |
| --- | --- | --- |
| Profile/auth | Luna profile worker | Profile registry, passwords, web sessions, web command |
| Transcript/speakers | Luna correction worker | Memory/app services and new HTTP handlers |
| Retrieval | Luna scale worker | semantic package and benchmark command |
| Forget/suppress | pi Luna forget worker | migration, memory/app domain, new HTTP handlers |
| Temporal history | pi Luna time worker | existing plane lifecycle and app/HTTP adapters |
| Integration | coordinator | Shared wiring, GUI coordination, queue/sync/audio fences, CLI wiring |
| Review | independent Luna workers | Auth/deletion and revision/retrieval regression checks |

Workers share the checkout and must preserve each other's changes. Migration
registration and shared interfaces are coordinated. pi overload retries use
15/30/60-second backoff and reduced concurrency, then native Luna fallback.

## Required acceptance

- Revision conflict preserves draft; old evidence stays stale; speaker bindings
  do not transfer silently across revisions.
- Profile isolation covers API, tasks, audio, indices and browser state.
- Forget is tombstone-first, resumable and idempotent. In-flight publication and
  replay cannot resurrect sources. Missing audio timing cannot silently widen
  a deletion. Completion requires managed content copies to be removed.
- AI-note suppression preserves source and blocks the same source/kind across
  revisions. It is not represented as forgetting the original fact.
- Fact history distinguishes real-world validity and server record time;
  unknown legacy time is not invented. Proposed statements stay unconfirmed.
- Exact retrieval remains available. ANN must validate authoritative scope,
  current-state, ambient and forgetting constraints before returning results.
- Benchmark synthetic 10k/100k/1m vectors at 256/768 dimensions independently
  from embedding semantics; exact-oracle recall targets .95@10 and .98@40.
- Use synthetic/temp data for tests and browser playback. Record PASS, FAIL,
  NOT_RUN or BLOCKED without turning unexecuted checks into successes.

## Rollback constraints

Additive migrations are tested on copies. Sidecars are disposable. Once a
forget operation has run, old binaries and old backups cannot be used to bypass
tombstones; recovery must reapply deletion fences. Cloud, manual exports and
unmanaged OS backups are outside the managed local purge.

## Profile migration and retirement

Create a new profile with `context-profile create --name NAME`; supply its
password through `MEMORY_PROFILE_PASSWORD` (or the configured password-env
variable). Adopt an existing local database with
`context-profile adopt --name NAME --db PATH [--audio-dir PATH]
[--events-db PATH]`. Adoption opens each source database read-only and creates a
consistent `VACUUM INTO` snapshot in a newly created profile. It copies only
regular files from the explicitly supplied audio root, rewrites stored sync
paths into that profile's audio directory, and never changes the source
checkout. It does not merge into an existing profile or copy rebuildable
indices/backups. If stored audio paths exist, `--audio-dir` is required.

The profile picker offers permanent retirement by requiring the exact profile
name and its current password. A durable retired-name catalog prevents later
recreation. The profile database retains only its retirement marker, and the
managed audio, task, plane-event, index, and backup contents are purged. The
empty profile directory and password verifier remain so an interrupted purge
can be retried; they do not encrypt the profile's files. Cloud copies, manual
exports, and unmanaged operating-system backups remain outside this local
purge.
