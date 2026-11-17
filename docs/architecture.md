# Architecture

This document is the contract for ward. A change in behavior updates this document in the same pull request. A section marked Planned is intent only. It is not built and it binds nothing until a change moves it into the contract.

## Goals and non-goals

Goal: ward is the forge layer on top of [motte](https://github.com/majorcontext/motte). motte is mechanism only: objects, refs with generation compare-and-swap, protected refs, ref-change events, git smart HTTP and the batched server-side land. ward holds the policy and the workflow: change requests, review, the merge queue, CI orchestration and mirroring to external git hosts.

Non-goals:

- No storage of git objects or refs. motte stores them. ward keeps a clone of a repository only as a disposable cache.
- No compute. ward runs no build, test or CI job itself. A compute service runs jobs (see CI orchestration).
- No user accounts. A caller authenticates with a signed token, and ward verifies it (see Caller token). ward keeps no user table and no password.
- No change to motte. ward uses the motte interfaces that `docs/architecture.md` of motte lists, and it adds none.
- No multi-region mirror. One ward service serves the motte store that it is configured for.

## Components

| Component | Responsibility | Phase |
|---|---|---|
| `cmd/ward` | Process: configuration, HTTP listener, workers. | W1 (prints its version) |
| `internal/token` | Caller token verifier (see Caller token). | W2 |
| `internal/mirror` | The mirror (see Mirror): trigger intake, state, clone cache, push workers. | W2 |
| `internal/leaklint`, `internal/testtime`, `internal/lint` | Repository tooling: denylist scan, test time budget, wall-clock analyzer. | W1 |
| Change requests, review, merge queue, CI orchestration, import host | See Planned work. | Not built |

ward imports the public packages of motte: `api` (wire types), `grant` (to sign the grants that ward sends to motte) and `client` (the HTTP client). It never imports an internal package of motte.

## Dependency direction

```
UI or caller --> ward --> motte
                   |
                   +----> compute service (job API, Planned)
                   |
                   +----> external git remote (mirror push)
```

ward calls motte, and for CI jobs it calls the job API of a compute service. Nothing depends on ward except a UI or a caller that renders or drives its API. motte never calls ward as a dependency: a motte subscription delivers events to a receiver URL, and the receiver is a route of ward that motte knows only as text.

## Service

`cmd/ward` reads its configuration from the environment:

| Variable | Meaning |
|---|---|
| `WARD_LISTEN` | Listen address. Default `:8080`. |
| `WARD_DATA_DIR` | Directory of the clone cache. Required. |
| `WARD_MOTTE_URL` | Base URL of motte. Required. |
| `WARD_MOTTE_KID` | `kid` that ward puts in the grants that it signs. Required. |
| `WARD_MOTTE_KEY_FILE` | Path of the Ed25519 private key that signs those grants. Required. |
| `WARD_PUBLIC_URL` | Base URL under which motte reaches ward. The receiver URL is this value plus `/v1/hooks/motte`. Required when a mirror is configured. |
| `WARD_HOOK_SECRET_FILE` | Path of the file that holds the subscription secret (16 to 256 bytes). Required when a mirror is configured. |
| `WARD_TRUSTED_KEYS` | JSON list of `{"kid","key","scopes"}` for caller tokens. `key` is the standard base64 of an Ed25519 public key. `scopes` holds only `read` and `operator`. The service refuses to start on any other scope text. |
| `WARD_MIRRORS` | JSON list of mirror entries (see Mirror, Configuration). Optional. |
| `WARD_CLONE_DISK_BUDGET` | Bytes that the clone cache may use. Default 20 GiB. |
| `WARD_CLONE_IDLE_TTL` | Idle time after which a clone is deleted, as a Go duration. Default `168h`. |
| `WARD_POLL_INTERVAL` | Interval of the event poll and of the subscription check. Default `30s`. |
| `WARD_VERIFY_INTERVAL` | Interval of the periodic Reconcile pass. Default `1h`. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Optional. Enables OTLP trace export. |

The key of `WARD_MOTTE_KEY_FILE` must be a trusted key of motte (`MOTTE_TRUSTED_KEYS` of motte) with the scopes `operator` and `read`. A later feature that lands through motte adds the scope `merge`.

`GET /health` answers 200. `GET /ready` answers 200 while the state store answers a ping. `GET /metrics` serves Prometheus metrics. These routes need no token. A path or method that no route serves answers 404 `not_found` or 405 `method_not_allowed` in the error envelope.

The state store keeps the mirror state (see Mirror, State). It is a transactional store behind an interface. The backend is chosen when the mirror is built, and the choice updates this section.

## Caller token

A caller token has the same encoding as a motte grant: `wd1.<base64url payload>.<base64url signature>`, with an Ed25519 signature over the base64url payload text and no padding. The payload is JSON.

| Claim | Meaning |
|---|---|
| `kid` | Key identifier. The verifier selects the public key by `kid`. |
| `scope` | `read` or `operator`. |
| `repo` | Repository text of motte. Required for `read`. |
| `subject` | The caller identity, for logs and traces only. It grants nothing. |
| `exp` | Expiry as Unix seconds. A token is valid while `now < exp`. |

`read` names one repository and may read its mirror status. `operator` may read every status and may trigger a repair. The verifier holds only public keys, each with a set of allowed scopes. Every rejection answers 401 `unauthorized` with the same body, and the response never carries the token. ward takes the repository from the token, never from an unchecked request field.

## Errors

The error body is `{"error":{"code","message","details"}}`. Codes: `unauthorized` (401), `forbidden` (403), `not_found` (404), `invalid_body` (400), `method_not_allowed` (405), `internal_error` (500). A panic in a handler is logged with its stack and answers 500 `internal_error` when the handler sent no status yet.

## Mirror

The mirror is the first feature. It keeps a one-way mirror from a motte repository to an external git remote, such as a hosted git service. It is for repositories where motte is the source of truth. The remote is a copy: ward overwrites whatever it holds under a mirrored name.

### Why a mirror service

Pushing from each workspace has these faults:

- A workspace holds a partial clone. A push from it needs a transport repository, and it fetches missing blobs from a promisor while it packs.
- Each workspace needs the credential of the remote.
- Each workspace retries on its own, and a workspace that stops before the push succeeds leaves the remote behind.

The mirror removes all three. It pushes from one full clone that ward owns, with one credential in one place, and it retries from durable state. A workspace only writes to motte.

### Configuration

`WARD_MIRRORS` is a JSON list. An entry has these fields:

| Field | Meaning |
|---|---|
| `name` | Unique name of the mirror. It keys the state. 1 to 64 characters of `a-z 0-9 -`. |
| `repo` | Repository text of motte, as in the `repo` claim of a grant. |
| `remote` | HTTPS URL of the remote repository. It holds no user information. The service refuses to start otherwise. |
| `refs` | Non-empty list of ref patterns. See Which refs. |
| `username` | User name for HTTP Basic on the remote. Optional. Default `x-access-token`. |
| `credential_file` | Path of a file that holds the password or token of the remote. Required. |
| `prune_unknown` | Optional. Default false. See Reconcile. |

Two entries may name the same `repo`. They share one clone and have separate state, a separate remote and separate failures. The service refuses to start on a `name` that repeats, a `refs` list that is empty or has an invalid pattern, or an unreadable `credential_file`. ward reads the credential file at each push, so a rotated credential applies to the next push with no restart.

### Which refs

A pattern has the rule of motte protected refs: a ref name, or a prefix that ends in `*`. `release/*` matches every ref that starts with `release/`, and `*` matches every ref. A pattern matches the stored ref name of motte. A list has at most 100 patterns.

A stored ref name maps to the git ref name that git clients see (see Git smart HTTP of motte): `main` is `refs/heads/main`, and a stored name that starts with `refs/` has that name. The mirror pushes to the same git ref name on the remote. A stored name under `refs/heads/` has no git name and is never mirrored.

A ref that no pattern matches is never created, changed or deleted on the remote. ward does not set the default branch of the remote.

### Triggers

A trigger says that one ref of one repository may have changed. The mirror reads the real value of the ref when it acts on a trigger. It never trusts the `old` or `new` of an event. Three sources produce triggers, and they produce the same record, so a repeat is harmless:

- Delivery: motte delivers each ref event to `POST /v1/hooks/motte` (see Ref events of motte).
- Poll: ward reads `GET /v1/events?cursor=C` (see below) and makes a trigger of each event.
- Reconcile: ward makes a trigger of each ref that needs one.

Delivery. At start, and when the subscription is missing, ward registers one subscription at motte with `POST /v1/subscriptions` and `{url, secret}`. It sets no `repo` filter, so one subscription serves every mirror. It keeps the subscription `id` and the `cursor` of the answer in the state store. A subscription of motte starts at the newest event, so it carries no history.

The receiver does these steps:

1. Read the body, at most 64 KiB.
2. Compute `sha256=` and the lowercase hex HMAC-SHA256 of the exact body under the secret, and compare it with `X-Motte-Signature` in constant time. A difference answers 401 `unauthorized` and stores nothing.
3. Decode the event. If `repo` and `ref` match no mirror entry, answer 204 and store nothing. This is required: any other answer would stall the partition of motte.
4. In one transaction of the state store, record the trigger and the pair `(partition, seq)` as the last event of that ref, then answer 204. If the transaction fails, answer 500.

The receiver answers before any git work. A slow push of one ref would otherwise block motte from delivering every other ref of its partition, because motte sends no later event of a partition before it gets a 2xx for the earlier one. Motte delivers at least once. A repeated `(partition, seq)` makes a trigger that already exists, so the receiver treats it as a repeat.

Poll. ward keeps a cursor of `GET /v1/events` in the state store. Every `WARD_POLL_INTERVAL` and at start, it reads pages with `limit=1000` until a page has no events. For each page, it records the triggers and the new cursor in one transaction. The cursor starts from the `cursor` of the subscription answer. Poll finds what delivery missed, such as events that arrived while ward was down.

### Catch-up after downtime

While ward is down, motte keeps its events (14 days by default, see Retention of motte) and retries delivery with a backoff of at most 5 minutes. On start, ward polls from its stored cursor, so it finds every event after its last durable position, whether or not a delivery arrives. It then works the triggers.

A poll that answers 410 `vcs_events_pruned` means the stored cursor fell below the retention floor, and the events between are lost. ward then does a full Reset:

1. Delete its subscription at motte, if it has one, with `DELETE /v1/subscriptions/{id}`.
2. Register a new subscription, and store its `id` and `cursor`.
3. Reconcile every mirror.

Step 2 comes before step 3, so an event that commits after the new subscription is not lost. A subscription whose own cursor fell below the floor stops delivering, and the log of motte names the error. ward finds this because its poll fails or because its subscription is missing from `GET /v1/subscriptions` at the periodic check (every `WARD_POLL_INTERVAL`), and it does the same Reset.

### Reconcile

Reconcile makes a trigger of every ref that may differ. It reads the stored refs of motte with `GET /v1/vcs/refs` and keeps those that a pattern matches. It adds every ref of the mirror state that motte no longer lists, so the delete reaches the remote. It lists the remote refs with `git ls-remote`, and for each ref that a pattern matches and that motte does not list:

- With `prune_unknown` false, it leaves the ref alone.
- With `prune_unknown` true, it makes a trigger for it, and the action deletes it.

Reconcile runs at the first start of a mirror, after a Reset, and every `WARD_VERIFY_INTERVAL`. The periodic pass also repairs drift: it compares the remote tip of each ref with the `pushed` value in the state, and when they differ it makes a trigger. An operator can start a pass for one mirror with `POST /v1/mirrors/{name}/reconcile`.

### State

The state store holds these records:

| Record | Fields |
|---|---|
| Mirror | `name`, `subscription_id`, `events_cursor`. |
| Ref of a mirror | `mirror`, `ref` (stored name), `last_partition`, `last_seq`, `dirty` (boolean), `pushed` (commit or empty), `attempts`, `next_attempt`, `failure` (class, or empty), `failure_since`, `pushed_at`. |

A trigger sets `dirty` on the ref record, and it updates `last_partition` and `last_seq` when its event is newer, by `seq` in one partition. A worker clears `dirty` only in the transaction that records a push result, and only when no trigger arrived after it read the ref. A trigger that arrives during a push therefore leaves the ref dirty, and the worker acts again. The state is small and holds no secret. The clone cache is not state: ward can lose all of it.

### Action on a ref

A worker acts on one dirty ref as follows:

1. Read the ref from motte with `GET /v1/vcs/refs/{name...}` and a `read` grant (see Git access). A 404 `vcs_not_found` means the ref is deleted, and the target is deletion.
2. If the target is a commit, make sure that the clone holds it. See Clone.
3. Push to the remote. For a commit, push `+<commit>:<git ref name>`. The `+` forces the update: the mirror copies the ref exactly, and a non-fast-forward move is a normal case, because agents rewrite history. For a deletion, push `:<git ref name>`. A remote that says the ref does not exist is a success.
4. On success, record `pushed` (the commit, or empty for a deletion) and `pushed_at`, clear `failure` and `attempts`, and clear `dirty` as State describes.

Every push is idempotent. Pushing a commit that the remote ref already holds changes nothing, and so does deleting a missing ref. A repeat of any step after a crash is therefore safe. This gives at-least-once pushes with no duplicate effect.

Ordering. The workers hold one lock for each `(mirror, ref)`, so two pushes of one ref never run together. Because step 1 reads the current ref, a later push never installs an older commit than an earlier push, whatever the order of the triggers. The order of the events of one ref is also fixed by motte: they share a partition and follow commit order. The mirror needs no order between two refs and assumes none. A worker takes the dirty refs of one repository together and sends them in one `git push` with one refspec for each ref. Git reports the result of each ref, and the worker records each result alone. The push is not atomic.

Read after write. A worker reads the ref from the metadata store of motte, so a read that follows an event shows that event or a later write. ward assumes this. If a read shows a ref older than the event that made the trigger, the push is a repeat of an older push, and the later event, which motte also delivers, corrects it.

### Git access

ward reaches motte through git smart HTTP (see Git smart HTTP of motte) with a `read` grant. ward signs the grant with its own key: `repo` is the repository text, `scope` is `read`, and `exp` is 15 minutes after signing, so that one `git fetch` cannot outlive it. The run of git has a 10 minute limit. ward passes the grant as `Authorization: Bearer` through the environment of the git process (`GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_n` and `GIT_CONFIG_VALUE_n` set `http.extraHeader`). It never puts the grant, the remote credential or a URL with user information in an argument list, a file of the clone or a log line. The remote credential reaches git through `GIT_ASKPASS` with a helper that is the `ward` binary and reads the credential file.

The URL of the repository is `<WARD_MOTTE_URL>/git/<prefix>/<path>.git`, for the repository text `<prefix>:<path>`. For the calls to the control plane of motte (`/v1/events`, `/v1/subscriptions`) ward signs an `operator` grant with a life of 60 seconds. For `GET /v1/vcs/refs` it uses a `read` grant.

### Clone

ward keeps one bare clone for each repository text under `WARD_DATA_DIR/clones/`. The clone is full: ward never uses `--filter`, `--depth` or `--single-branch`. A push from a partial clone would need a promisor to fetch blobs while it packs, and the remote needs every object that the pushed commit reaches. motte offers no `shallow`, `deepen` or other filter than `blob:none`, and this agrees with that.

To get a commit, a worker fetches it by object name: `git fetch <motte URL> <commit>`. A `want` may name any object of the repository (see Git smart HTTP of motte), and a fetch by name needs no ref, so a ref that moves again cannot make it fail. The fetch speaks protocol v2, which motte requires (`Git-Protocol: version=2`). The worker then sets the local ref `refs/mirror/<git ref name>` to the commit, so that the objects stay reachable until the next push. After a deletion succeeds, the worker removes that local ref.

motte sends whole objects with no deltas, so a first fetch is larger than a repacked clone. The worker runs `git gc --auto` after each push batch.

If a git run fails in a way that is not a network, motte or remote error, such as a corrupt object, ward deletes the clone and acts again once. This is always safe, because the clone is a cache.

### Disk use and pruning

- A clone that has no dirty ref and no trigger for `WARD_CLONE_IDLE_TTL` is deleted. The next trigger fetches it again.
- When the cache uses more than `WARD_CLONE_DISK_BUDGET`, ward deletes idle clones, least recently used first, until it fits. A clone that a worker uses, or that has a dirty ref, is never deleted. If the budget is still exceeded, ward keeps working, reports `ward_mirror_disk_over_budget` and logs it.
- Every 24 hours ward runs `git gc --prune=now` on each clone, which frees the objects of deleted refs.

### Failure and backoff

A failed action leaves the ref dirty, increments `attempts` and sets `next_attempt`. The wait is 1 second, doubled after each attempt, up to 5 minutes, with random jitter of up to 20 percent. The cap holds until the action succeeds. A new trigger for the ref sets `next_attempt` to now, so fresh news is not held by an old backoff. One failing ref delays no other ref.

A failure has one class, which the status shows:

| Class | Cause |
|---|---|
| `motte` | The read of the ref or the fetch from motte failed, or motte answered 401 or 403. |
| `remote_unreachable` | The connection to the remote failed or timed out. |
| `remote_auth` | The remote refused the credential (HTTP 401 or 403 on the connection). |
| `remote_rejected` | The remote refused the ref update, such as a protected branch, a hook or a rule against force pushes or deletes. ward does not retry with a different command, because the mirror copies the ref exactly. |
| `local` | Any other failure of git or of the clone. |

The class and a short message are in the status. The message never holds a URL, a credential or a body.

### Status API

`GET /v1/mirrors` lists the mirrors, and `GET /v1/mirrors/{name}` returns one with its refs. Both need an `operator` token. A `read` token reads `GET /v1/mirrors/{name}` only when the `repo` of the token equals the `repo` of the mirror, else 403 `forbidden`. The answer for a mirror:

```json
{
  "name": "...",
  "repo": "...",
  "state": "synced | pending | failing",
  "refs": [
    {"ref": "main", "state": "synced", "pushed": "<commit>", "pushed_at": "<RFC 3339>",
     "failure": "", "attempts": 0, "failure_since": ""}
  ]
}
```

A ref is `synced` when it is not dirty, `pending` when it is dirty with no failure, and `failing` when it has a failure class. The mirror takes the worst state of its refs. A list of refs has at most 1000 entries, with `?cursor=` as the opaque continuation.

`POST /v1/mirrors/{name}/reconcile` needs an `operator` token and answers 202. It makes the triggers of a Reconcile pass and returns at once.

### Observability

- Metrics, with names that start with `ward_`: `ward_mirror_pushes_total` (by mirror and result), `ward_mirror_dirty_refs` (by mirror), `ward_mirror_failing_refs` (by mirror and class), `ward_mirror_lag_seconds` (age of the oldest dirty ref, by mirror), `ward_mirror_clone_bytes`, `ward_mirror_disk_over_budget`, `ward_hook_events_total` (by result) and `ward_http_requests_total` by route.
- Traces: OTLP, with the endpoint from the environment and no vendor in the code. Span names start with `ward.mirror.` for the work (`ward.mirror.fetch`, `ward.mirror.push`).
- Logs: JSON. A log line never holds a grant, a token, the hook secret, a remote credential, a remote URL or an event body.
- Instrumentation is additive. It never changes behavior or errors.

### Tests

Contract tests start ward against a real motte (the `testserver` package of motte) and a local bare git repository as the remote. They cover: a push, a force push, a delete, a ref that no pattern matches, a repeated delivery, a delivery with a bad signature, a poll after downtime, a Reset after a 410, and a remote that rejects a ref. Waits use the injected clock.

## Planned work

Nothing in this section is built. Each paragraph names the intent and the motte mechanism that it uses.

### Change requests

A change is a stable change ID plus its current commit. An agent rewrites a change many times, so ward records each rewrite as a revision of the same change and keeps the history of revisions. The change ID comes from the `change-id` header that a commit carries and that a land keeps (see Land of motte). A change request is the proposal to land a change on a target ref. It uses motte refs and ref events: a change moves when its ref moves.

### Review threads and approvals

A review thread is a comment on a path and a line of a revision, with replies and a resolved flag. An approval names a revision and a reviewer. ward carries a thread to the next revision when the lines are unchanged. Approvals are policy of ward and use no motte mechanism.

### Merge queue

The queue orders approved change requests for a target ref. It holds a change until its required checks pass. It lands a batch of changes together and tests the batch first. It lands through `POST /v1/repos/{repo}/lands` with a `merge` grant (see Land of motte), so the land uses the conflict check and the rebase of motte and needs no compare-and-swap loop in ward. When a batch fails, the queue bisects it: it splits the batch and tests each half until it finds the change that fails, and it removes that change and lands the rest. The target ref is a protected ref of motte, so only a `merge` grant moves it (see Protected refs of motte).

### CI orchestration

A pipeline file in the repository lists jobs, their dependencies and their required status. ward reads the file at the commit under test, creates a run, and starts each job through the job API of a compute service. ward holds the run state and the check result, and the compute service holds the execution. Ref events from motte start the runs.

### Vanity Go import host

ward answers the `go-import` meta tag for a configured domain, so `go get` for a vanity import path resolves to the git smart HTTP URL of a motte repository (see Git smart HTTP of motte). The host serves the meta tag only, and the `go` tool fetches the code from motte with a credential that the caller supplies.

## Versioning

The path prefix `/v1` is additive. New fields and new routes are allowed, and clients ignore unknown fields. A breaking change gets `/v2`, served beside `/v1`. The module is tagged with semantic versions.
