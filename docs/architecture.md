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
| `cmd/ward` | Process: `ward` serves, `ward version` prints the version, `ward land` lands a branch (see Land), `ward token` and `ward login` manage the credential of a person (see Human identity), `ward check` runs the checks of a working tree locally (see Local check). It also serves as the `GIT_ASKPASS` helper (see Git access). | W2, W3 |
| `internal/config` | Reads the environment (see Service). | W2 |
| `internal/token` | Caller token verifier (see Caller token). | W2 |
| `internal/identity` | The seam that turns the credential of a request into claims, its suite and its three implementations, and the per-user token store (see Provider seams). | W5 |
| `internal/server` | HTTP routes, error envelope, token checks, status API, land route. | W2, W3, W4 |
| `internal/land` | The land (see Land): merge base, the land queue, the candidate and the call to motte. | W3, W5 |
| `internal/check` | Checks (see Checks): the check file, the result store, and the delivery of check requests to the runner. | W5 |
| `internal/mirror` | The mirror (see Mirror): trigger intake, clone cache, push workers, GitHub App credential, seed. `internal/mirror/sqlite` is its state store. | W2, W3, W4 |
| `internal/telemetry` | OTLP trace export. | W2 |
| `internal/leaklint`, `internal/testtime`, `internal/lint` | Repository tooling: denylist scan, test time budget (enforced in CI's reference environment, not in the land check), wall-clock analyzer. | W1 |
| `images/ward`, `charts/ward`, `.github/workflows/image.yml` | Container image, Helm chart and image release (see Release). | W3 |
| Change requests, review, merge queue, CI orchestration, import host | See Planned work. | Not built |

ward imports the public packages of motte: `api` (wire types), `grant` (to sign the grants that ward sends to motte) and `client` (the HTTP client). It never imports an internal package of motte. Tests also import `testserver` and `meta` of motte.

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
| `WARD_MOTTE_KEY_FILE` | Path of the file that holds the Ed25519 private key that signs those grants, as the standard base64 of its 64 bytes or of its 32-byte seed. Required. |
| `WARD_PUBLIC_URL` | Base URL under which motte reaches ward. The receiver URL is this value plus `/v1/hooks/motte`. Required when a mirror is configured. |
| `WARD_HOOK_SECRET_FILE` | Path of the file that holds the subscription secret (16 to 256 bytes, with a final line break removed). Required when a mirror is configured. |
| `WARD_TRUSTED_KEYS` | JSON list of `{"kid","key","scopes"}` for caller tokens. `key` is the standard base64 of an Ed25519 public key. `scopes` holds only `read`, `operator`, `land` and `check`. The service refuses to start on any other scope text. |
| `WARD_OIDC_ISSUER` | Optional. The exact `iss` claim of an OIDC issuer. Turns on OIDC identities on the land routes (see Human identity). Off by default. |
| `WARD_OIDC_AUDIENCE` | The `aud` claim. Required with the issuer. |
| `WARD_OIDC_RULES` | JSON list of `{"principals","repos"}`. A principal is a claim value (an email by default) or `@domain`. `repos` holds repository texts. Required with the issuer. |
| `WARD_OIDC_KEYS_URL` | Optional. Address of the key set. Default: the `jwks_uri` of `<issuer>/.well-known/openid-configuration`. |
| `WARD_OIDC_HEADER` | Optional. Header that carries the JWT. Default `Authorization` (a Bearer token, or the password of Basic). Any other header carries the bare JWT. |
| `WARD_OIDC_CLAIM` | Optional. Claim that the rules match. Default `email`. |
| `WARD_OIDC_ALGS` | Optional. Comma-separated algorithms from `RS256`, `RS384`, `RS512`, `ES256`, `ES384`. Default `RS256`. |
| `WARD_TLS_CERT`, `WARD_TLS_KEY` | Optional. PEM files. When both are set ward serves TLS itself. When neither is set it serves plain HTTP for a reverse proxy, a tunnel or a local run. One without the other is an error. |
| `WARD_MIRRORS` | JSON list of mirror entries (see Mirror, Configuration). Optional. |
| `WARD_RUNNERS` | JSON list of runner entries (see Checks, Runner). Optional. A repository with a check file and no runner fails each land onto that ref with `land_invalid`. |
| `WARD_GITHUB_API_URL` | Base URL of the GitHub API for the GitHub App credential. An `http` or `https` URL. Default `https://api.github.com`. |
| `WARD_STATE_FILE` | Path of the SQLite file of the state store. Default `WARD_DATA_DIR/state.db`. |
| `WARD_CLONE_DISK_BUDGET` | Bytes that the clone cache may use. Default 20 GiB. |
| `WARD_CLONE_IDLE_TTL` | Idle time after which a clone is deleted, as a Go duration. Default `168h`. |
| `WARD_SEED_MAX_OBJECTS` | Most objects that one push of a seed may add (see Seed). Default 50000. |
| `WARD_SEED_MAX_BYTES` | Most packed bytes that one push of a seed may add. Default 33554432 (32 MiB). |
| `WARD_SEED_MAX_INFLATED_BYTES` | Most bytes of whole objects (the sum of the object sizes) that one push of a seed may add. Default 33554432 (32 MiB). |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Optional. Enables OTLP trace export. |
| `GIT_SSL_CAINFO` | Optional. ward passes it to git, so that git trusts a private certificate authority of a remote. |

The key of `WARD_MOTTE_KEY_FILE` must be a trusted key of motte (`MOTTE_TRUSTED_KEYS` of motte) with the scopes `operator` and `read`. The land (see Land) and the seed (see Seed) also need the scope `merge`, so that key must hold `operator`, `read` and `merge`.

`GET /health` answers 200. `GET /ready` answers 200 while the state store answers a ping. `GET /metrics` serves Prometheus metrics. These routes need no token. A path or method that no route serves answers 404 `not_found` or 405 `method_not_allowed` in the error envelope.

The state store keeps the mirror state (see Mirror, State). It is a transactional store behind the interface `mirror.Store`. The backend is SQLite, through a pure-Go driver, so the binary needs no cgo. It runs in WAL mode on one connection, and each write is one transaction. `WARD_STATE_FILE` should name a durable path: the clone cache may be lost, but the state should not be. When no mirror is configured, `/v1/mirrors` lists none and the hook routes answer 404 `not_found`.

## Caller token

A caller token has the same encoding as a motte grant: `wd1.<base64url payload>.<base64url signature>`, with an Ed25519 signature over the base64url payload text and no padding. The payload is JSON.

| Claim | Meaning |
|---|---|
| `kid` | Key identifier. The verifier selects the public key by `kid`. |
| `scope` | `read`, `operator`, `land` or `check`. |
| `repo` | Repository text of motte. Required for `read`, `land` and `check`. |
| `subject` | The caller identity, for logs and traces only. It grants nothing. |
| `exp` | Expiry as Unix seconds. A token is valid while `now < exp`. |

`read` names one repository and may read its mirror status. `operator` may read every status and may trigger a repair. `land` names one repository and may land a branch of it and read its lands (see Land); it reads no status. `check` names one repository and may post check results of it (see Checks); it does nothing else. The verifier holds only public keys, each with a set of allowed scopes. Every rejection answers 401 `unauthorized` with the same body, and the response never carries the token. ward takes the repository from the token, never from an unchecked request field.

## Human identity

A person lands with no caller token in one of two ways, and neither needs a vendor. Each is an `identity.Authenticator`, tried after the caller token. A human identity gives only the scope `land` on a listed repository, with the principal as `subject`. It reads no status, posts no check result, triggers no repair, mints no token and never gets a grant of motte. ward lands with its own `merge` grant, so `main` stays land-only.

Only `POST /v1/lands` and `GET /v1/lands/{id}` accept a human identity. The repository is the `repo` of the body, or the repository of the land that `{id}` names. An unknown `id` answers 401 for a human identity, so an id is no oracle.

OIDC identity (`internal/identity/oidc`). ward verifies a JWT from the issuer of `WARD_OIDC_ISSUER` and trusts no header that is not a valid JWT.

- The JWT must use an algorithm of `WARD_OIDC_ALGS`, be signed by a key of the key set (cached for 1 hour; a `kid` that the cache does not hold causes one new fetch a minute; a cache that is 1 hour past its last good fetch is refused), with `iss` equal to the issuer, `aud` holding the audience, `now < exp`, `nbf <= now` and a non-empty value in the claim `WARD_OIDC_CLAIM`. Any failure answers 401 `unauthorized`.
- A rule of `WARD_OIDC_RULES` lists the principals that may land on each repository. No matching rule answers 401. The match ignores case for the claim `email` and is exact for any other claim.
- An identity-aware proxy that signs a JWT in its own header is one configuration of the same verifier: set `WARD_OIDC_HEADER`, `WARD_OIDC_ISSUER`, `WARD_OIDC_AUDIENCE` and `WARD_OIDC_KEYS_URL` to its values. For Cloudflare Access the header is `Cf-Access-Jwt-Assertion`, the issuer is `https://<team>.cloudflareaccess.com` and the key set is at `https://<team>.cloudflareaccess.com/cdn-cgi/access/certs`.

Per-user tokens (`internal/identity/usertoken`). They need no issuer. An operator issues one with `POST /v1/tokens` and `{principal, scope, repos, ttl_seconds}`, or with `ward token create --principal P [--scope land] --repo R [--repo R]... [--ttl D]`. `scope` is `land` and `repos` holds at least one repository. Only a signed caller token with the scope `operator` reaches the three token routes: a human identity, and any other token, cannot issue, list or revoke.

- A token is `wdu_` and 43 characters of base64url from 32 random bytes. The answer 201 `{token, user_token}` shows it once. The state store keeps the SHA-256 hex of it with the ID `ut_<16 hex>`, the principal, the scope, the repositories, the creation time, the expiry and the revocation time (table `user_tokens`, created with the other tables at open).
- `GET /v1/tokens` lists the records, newest first, with no token text. `DELETE /v1/tokens/{id}` revokes and answers 204, or 404 `not_found` for an unknown ID. A repeat keeps the first revocation time.
- A token is accepted as `Authorization: Bearer <token>` or as the password of HTTP Basic. A token that is unknown, revoked, past its expiry or not listed for the repository answers 401.
- `ward token list` and `ward token revoke ID` call `GET /v1/tokens` and `DELETE /v1/tokens/{id}`. The commands read `WARD_URL` and an operator token from `WARD_TOKEN`, or sign one with `WARD_KEY_FILE` and `WARD_KID`.

`ward login` runs the OAuth device authorization flow (RFC 8628) against `WARD_OIDC_ISSUER` with the client `WARD_OIDC_CLIENT_ID`. It reads the endpoints from the discovery document, polls the token endpoint with `slow_down` and `authorization_pending` handled, and saves the ID token (else the access token) in `<user config dir>/ward/login.json` with mode 0600. `ward login --print` prints it. `ward land` uses it when `WARD_TOKEN` and the key file are unset.

The local loop of a person needs no identity provider, no tunnel and no box: an operator token, `ward token create`, plain `git` with the token as the password, `ward check` and `ward land` (see README, Developing ward). ward serves TLS itself when `WARD_TLS_CERT` and `WARD_TLS_KEY` are set, and no code assumes a proxy or a tunnel.

## Provider seams

| Seam | Interface | Suite | Implementations | Gap |
|---|---|---|---|---|
| identity | `internal/identity.Authenticator` | `internal/identity/identitytest.Run` | `internal/identity/bearer`, `internal/identity/oidc`, `internal/identity/usertoken` | |
| user token store | `internal/identity/usertoken.Store` | `internal/identity/usertoken/usertokentest.Run` | `internal/mirror/sqlite`, `usertokentest.Mem` | |

The suite `identitytest.Run` has an optional Revoked case for a credential that can be revoked. The header, the claim and the key set address of an issuer are configuration, and a vendor host name appears only in documents.

## Errors

The error body is `{"error":{"code","message","details"}}`. Codes: `unauthorized` (401), `forbidden` (403), `not_found` (404), `invalid_body` (400), `method_not_allowed` (405), `internal_error` (500), `land_conflict` (409, with `details.paths`), `land_invalid` (400) and `land_timeout` (504). A land that fails in the queue carries one of these codes, or `check_failed` or `land_stale`, in its `error` (see Land). A panic in a handler is logged with its stack and answers 500 `internal_error` when the handler sent no status yet.

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
| `credential_file` | Path of a file that holds the password or token of the remote. Required unless `github_app` is set, and then it must be absent. |
| `github_app` | Optional object `{app_id, installation_id, key_file}`. Makes a GitHub App installation the credential (see GitHub App credential). It excludes `credential_file` and `username`. |
| `prune_unknown` | Optional. Default false. See Reconcile. |
| `seed` | Optional. Default false. When true, ward copies the remote into an unseeded motte repository at the first start of the mirror (see Seed). |

Two entries may name the same `repo`. They share one clone and have separate state, a separate remote and separate failures. ward resolves `WARD_DATA_DIR`, `WARD_STATE_FILE` and each `credential_file` and `github_app.key_file` to absolute paths at start, because git runs with the clone as its working directory. The service refuses to start on a path that it cannot resolve, on a `name` that repeats, a `refs` list that is empty or has an invalid pattern, or an unreadable `credential_file`, or a `github_app` with an `app_id` or `installation_id` that is not positive or a `key_file` that is not a PEM RSA private key. ward reads the credential file at each push, so a rotated credential applies to the next push with no restart.

### GitHub App credential

With `github_app`, ward gets the password for the remote from a GitHub App installation instead of a file. The user name is `x-access-token`.

1. ward signs an app JWT with the RSA private key of `key_file` (PKCS#1 or PKCS#8 PEM), algorithm RS256. `iss` is the app id as text, `iat` is 60 seconds before now and `exp` is 9 minutes after now. ward reads the key file at each exchange, so a rotated key applies with no restart.
2. ward sends `POST {api}/app/installations/{installation_id}/access_tokens` with `Authorization: Bearer <JWT>`, where `{api}` is `WARD_GITHUB_API_URL`. It follows no redirect. It reads `token` and `expires_at` from the answer.
3. ward keeps the token in memory, one for each mirror, until 5 minutes before `expires_at`, by the injected clock. It exchanges again on demand, when a push, a stale re-check or a Reconcile of that mirror needs a token and the cached one is too old. It starts no timer for this.
4. For one git run, ward writes the token to a mode 0600 file in `WARD_DATA_DIR`, passes that file as `WARD_ASKPASS_FILE` (see Git access) and removes it when the run ends.

A 401 or 403 from the token endpoint is the failure class `remote_auth`. A network failure, a timeout, any other status or an answer that holds no token is `remote_unreachable`. A key file that cannot be used is `local`. The failure follows Failure and backoff, so a failure shows in the status of the refs. A message and a log line never hold the token, the JWT or the key.

### Which refs

A pattern has the rule of motte protected refs: a ref name, or a prefix that ends in `*`. `release/*` matches every ref that starts with `release/`, and `*` matches every ref. A pattern matches the stored ref name of motte. A list has at most 100 patterns.

A stored ref name maps to the git ref name that git clients see (see Git smart HTTP of motte): `main` is `refs/heads/main`, and a stored name that starts with `refs/` has that name. The mirror pushes to the same git ref name on the remote. A stored name under `refs/heads/` has no git name and is never mirrored.

Only branches and tags are mirrored. In motte, a branch is a stored name that is bare, that is, does not start with `refs/`, and a tag is a stored name under `refs/tags/`. Every other stored name, such as `refs/pull/<n>/head`, `refs/changes/...` or `refs/notes/...`, is never created, changed or deleted on the remote, whatever the patterns say. A pattern only filters within the branches and tags, so `*` matches every branch and every tag. Reconcile, the remote listing and the seed ignore every other name on both sides.

A ref that no pattern matches is never created, changed or deleted on the remote. ward does not set the default branch of the remote.

### Triggers

A trigger says that one ref of one repository may have changed. The mirror reads the real value of the ref when it acts on a trigger. It never trusts the `old` or `new` of an event. Three sources produce triggers, and they produce the same record, so a repeat is harmless:

- Delivery: motte delivers each ref event to `POST /v1/hooks/motte` (see Ref events of motte).
- Catch-up: ward reads `GET /v1/events` and makes a trigger of each event. See Catch-up.
- Reconcile: ward makes a trigger of each ref that needs one. See Reconcile.

ward has no timer that starts a poll or a check. The only waits are the retry backoff of a failed action (see Failure and backoff). Every read of motte or of the remote starts from an event, a start of the process, a stale rejection or an operator request.

Delivery. At start, ward lists `GET /v1/subscriptions`. If no subscription has its receiver URL and the `id` that the state store holds, ward deletes any subscription that has the receiver URL (the state was lost) and registers one with `POST /v1/subscriptions` and `{url, secret}`. It sets no `repo` filter, so one subscription serves every mirror, and the events of a partition reach the receiver with no gap that a filter would make. It keeps the subscription `id` in the state store. It keeps the `cursor` of the answer as `events_cursor` only when the state store holds none, so a catch-up still reads the events that a deleted subscription would have delivered. The code of motte seeds the cursors of a new subscription with the newest `seq` of every partition, so the subscription carries no history, and the first event that it delivers in a partition has the `seq` after that head.

The receiver does these steps:

1. Read the body, at most 64 KiB. A larger body answers 400 `invalid_body`.
2. Compute `sha256=` and the lowercase hex HMAC-SHA256 of the exact body under the secret, and compare it with `X-Motte-Signature` in constant time. A difference answers 401 `unauthorized` and stores nothing.
3. Decode the event. A body that is not an event answers 400 `invalid_body`. Compare its `seq` with `last_seq` of its partition in the state store (see State). A partition with no stored `last_seq` has no baseline: the first event sets it and starts no catch-up.
   - `seq` equal to `last_seq` + 1: in order.
   - `seq` at or below `last_seq`: a repeat. Answer 204 and store nothing.
   - `seq` above `last_seq` + 1: a gap. Store the event as in step 4, and mark the partition for a catch-up (see Catch-up).
4. In one transaction of the state store, set `last_seq` of the partition. If `repo` and `ref` match a mirror entry, also record the trigger. Then answer 204. If the transaction fails, answer 500. An event that matches no mirror entry still moves `last_seq`, and it answers 204. Any other answer would stall the partition of motte.

The receiver answers before any git work. A slow push of one ref would otherwise block motte from delivering every other ref of its partition, because motte sends no later event of a partition before it gets a 2xx for the earlier one. motte delivers at least once and in `seq` order in a partition, so a gap shows that ward lost state, for example a restore of the state store from a backup, or that motte lost events.

### Catch-up

Catch-up reads `GET /v1/events?cursor=C&limit=1000` with an `operator` grant, from the `events_cursor` that the state store holds, and repeats until a page has no events. The cursor is opaque, so ward passes it unchanged. For each page, ward records the triggers of the events that match a mirror entry and the new `events_cursor` in one transaction. It records `last_seq` of each partition from the events that it reads, if that is higher than the stored value. After the last page, the stored cursor is the head of motte at that read.

Catch-up runs only in these cases:

- At start, so ward finds the events that committed while it was down.
- When the receiver finds a gap in a partition. A catch-up for a gap reads from the stored `events_cursor`, which is at or before the last event that ward recorded in every partition, so it passes the gap. It also reads events that delivery already recorded, and those make triggers that exist. A catch-up that is already running covers a new gap that it has not passed yet, and a gap that it has passed needs no second run.

While ward is down, motte keeps its events (14 days by default, see Retention of motte) and retries delivery with a backoff of at most 5 minutes, so a short outage needs the catch-up of the start and little more.

An answer of 410 `vcs_events_pruned` means that the stored cursor fell below the retention floor and the events between are lost. An answer of 400 `invalid_body` means that the cursor is ahead of the head of motte, which happens when motte lost state. In both cases ward does a full Reset:

1. Delete its subscription at motte, if it has one, with `DELETE /v1/subscriptions/{id}`.
2. Register a new subscription, and store its `id` and `cursor` as the `events_cursor`. Clear `last_seq` of every partition.
3. Reconcile every mirror.

If a step of start, catch-up or Reset fails because motte or the network failed, ward retries it with the backoff of Failure and backoff, and the process keeps serving. Step 2 comes before step 3, so an event that commits after the new subscription is not lost. A subscription whose own cursor fell below the floor stops delivering, and the log of motte names the error. ward has no timer, so it finds this at the next start, because its subscription is missing or its catch-up fails, or because an operator sees `ward_mirror_lag_seconds` grow and calls `POST /v1/hooks/motte/reset`, which answers 202 and does a Reset. That route needs an `operator` token.

### Reconcile

Reconcile makes a trigger of every ref of a mirror that may differ. It reads the stored refs of motte with `GET /v1/vcs/refs`, which answers `{"refs":[{name, commit, generation}]}` for the repository of the grant, and it keeps the refs that a pattern matches. It adds every ref of the mirror state that motte no longer lists, so the delete reaches the remote. It lists the remote refs with `git ls-remote`, and for each ref that a pattern matches and that motte does not list:

- With `prune_unknown` false, it leaves the ref alone.
- With `prune_unknown` true, it makes a trigger for it, and the action deletes it.

For each remote ref that a pattern matches, Reconcile also sets `pushed` in the state to the remote tip, or to empty when the ref is absent, so the next push compares with what the remote holds (see Action on a ref).

A failed Reconcile leaves the mirror unreconciled and retries with the same backoff. The worker acts on dirty refs in the meantime. Reconcile runs only in these cases:

- At the first start of a mirror, that is when the state store has no record of its name.
- After a Reset.
- On the request of an operator: `POST /v1/mirrors/{name}/reconcile`. A request that comes before the worker of the mirror starts stays pending and the worker runs it when it starts.
- For one ref, when a push to the remote is refused as stale because the remote moved outside ward (see Action on a ref).

ward never compares the remote with the state on a timer. A change that someone makes on the remote outside ward stays until the next push of that ref, which the lease catches, or until an operator reconciles.

### Seed

A seed moves the source of truth of a repository from the remote into motte. motte creates a native repository with one ref, `main`, at a root commit with an empty tree. A seed copies the whole history of the remote into that repository once. After it, the mirror runs in its normal direction.

When a seed runs. A seed runs only for a mirror with `seed` true. It runs before the first Reconcile of the mirror, when the state store has no record of the mirror name, and it resumes at a start when the seed record is `started`. It never runs when the seed record is `done`. ward decides this, and gives the worker its seed request, before it starts the worker of the mirror. A worker whose seed is wanted does no Reconcile and no push to the remote until the seed is `done`, whatever dirty refs, pending Reconcile requests or events the state holds. A mirror that the state store already knows and that has no seed record never seeds, so a later `seed` true changes nothing.

Unseeded. A motte repository is unseeded when `GET /v1/vcs/refs` lists exactly one ref, `main`, and its commit has no parent (the object of the commit holds no `parent` line) and its tree is the empty tree (`GET /v1/vcs/commits/{sha}` answers the tree `4b825dc642cb6eb9a060e54bf8d69288fbee4904`). A seed of a repository that is not unseeded, with no seed record, is refused: ward writes nothing to motte or to the remote, the mirror state is `seed_refused` with a log line that says why, and the mirror does no work until the configuration or the repository changes and ward restarts. ward never overwrites a repository of motte from a remote.

Steps.

1. Set the seed record to `started` and the mirror state to `seeding`.
2. List the remote refs with `git ls-remote`. Fetch every branch (`refs/heads/*`, stored as a bare name) and tag (`refs/tags/*`) that a pattern matches (see Which refs) into the clone as `refs/mirror/<git ref name>`, with the credential of the mirror (a file or a GitHub App token), in batches of 200 refspecs.
3. Order the refs by the commit time of their tip, oldest first, then by name. For each ref, push to motte over git smart HTTP with a `merge` grant of 15 minutes. `main` has a lease on the commit that motte holds (`--force-with-lease=refs/heads/main:<root>`), and every other ref has a lease on absence, so it is a create. A ref that motte already holds at the tip is skipped, which makes a resumed seed repeat nothing.
4. Set the seed record to `done`, remove the mirror state `seeding`, count the seed and request a Reconcile. Reconcile finds motte and the remote equal.

A seed copies no other ref of the remote, such as `refs/pull/*`.

A failed step retries with the backoff of Failure and backoff, with the mirror in the state `seeding`. A retry continues where the failed run stopped, and it writes only refs that ward wrote.

Expected values. The state store holds, for each ref of the seed, the value that ward expects motte to hold: `commit`, the last value that ward wrote or found (empty for a ref that must be absent), and `next`, the value of a push that is under way and may have landed. The seed records `main` at the root commit when it starts. Before each push it records `next`, and after the push it records the value as `commit`. At each attempt, before any push, ward compares every ref of motte with these values. A ref that motte holds with a value that is neither `commit` nor `next`, a ref that has no record, or a recorded ref that motte lost, ends the seed as `seed_refused`. The status message names the ref. ward never overwrites a ref of motte that it did not write, and each push has a lease on the value that it expects.

Chunks. A push to motte has limits (see Git smart HTTP of motte): a body of 64 MiB and a pack of 100000 objects. motte also limits the inflated data of a pack (64 MiB). ward keeps each push under `WARD_SEED_MAX_OBJECTS` objects, `WARD_SEED_MAX_BYTES` packed bytes and `WARD_SEED_MAX_INFLATED_BYTES` bytes of whole objects, which default to half of those limits. For one ref, ward lists the commits of its first-parent history that no commit that motte holds reaches. It measures a push with `git rev-list --objects` as the objects, the packed bytes and the sum of the object sizes (`git cat-file --batch-check`) of the commit that no earlier push reaches. It pushes the newest first-parent commit that fits, then repeats from there, so each push moves the ref forward by a fast-forward and holds the objects that the commit reaches through all its parents. The planner finds each commit with a binary search, because the cost grows with the commit. The final push is the tip itself, and an annotated tag object follows its commit. A single commit that does not fit alone is pushed alone, and motte decides. A ref therefore shows intermediate commits in motte while its seed runs, and an event of motte follows each push.

Seed cleanup. An earlier version of ward seeded refs that are not branches or tags. At each start, for each mirror that has a seed record and no cleanup record, ward reads the refs of motte and, for each ref of the seed record that is not mirrorable (see Which refs), deletes it from motte with a push under a `merge` grant of 15 minutes and a lease on the value that motte holds. It deletes a ref only when that value is the `commit` or the `next` of the seed record. It skips and logs a ref that motte holds at another value, and a ref that the seed record does not list, so it never deletes a ref that ward did not write. Then, in one transaction, it removes those refs from the seed record and from the ref state of the mirror, and records the cleanup, so the cleanup runs once. A failed cleanup retries with the backoff of Failure and backoff before any worker starts.

The mirror does no push to the remote while it seeds, and a ref event that arrives during the seed is a trigger that the worker acts on after it.

### State

The state store holds these records:

| Record | Fields |
|---|---|
| Service | `subscription_id`, `events_cursor`, and `last_seq` for each partition (0 to 63). |
| Mirror | `name`, set when the first Reconcile of the mirror ends. |
| Seed | `mirror`, `state` (`started` or `done`), for each ref of the seed `commit` and `next`, and the cleanup record (see Seed). |
| Ref of a mirror | `mirror`, `ref` (stored name), `last_partition`, `last_seq`, `dirty` (boolean), `version`, `dirty_since`, `pushed` (commit, empty, or unset), `attempts`, `next_attempt`, `failure` (class, or empty), `message`, `failure_since`, `pushed_at`. |

A trigger sets `dirty` on the ref record, increments `version`, sets `dirty_since` when the ref was clean, sets `next_attempt` to now, and updates `last_partition` and `last_seq` of the ref record when its event is newer, by `seq` in one partition. `pushed` is unset until a push or a Reconcile gives it a value, and an empty value means that the ref is absent on the remote. A worker clears `dirty` only in the transaction that records a push result, and only when `version` is the value that the worker read before it read the ref, so no trigger arrived since. A trigger that arrives during a push therefore leaves the ref dirty, and the worker acts again. The state is small and holds no secret. The clone cache is not state: ward can lose all of it.

### Action on a ref

A worker acts on one dirty ref as follows:

1. Read the ref from motte with `GET /v1/vcs/refs/{name...}` and a `read` grant (see Git access). A 404 `vcs_not_found` means the ref is deleted, and the target is deletion.
2. If the target is a commit, make sure that the clone holds it. See Clone.
3. Push to the remote with a lease on the value that ward last pushed (`--force-with-lease=<git ref name>:<pushed>`, and an empty `pushed` means that the ref must be absent). For a commit, push `<commit>:<git ref name>` when a lease applies, and `+<commit>:<git ref name>` when none applies. A non-fast-forward move is a normal case, because agents rewrite history, and the mirror copies the ref exactly: git accepts a non-fast-forward update when the lease holds, and a leading `+` would make git ignore the lease. For a deletion, push `:<git ref name>`. A remote that says the ref does not exist is a success. A ref whose `pushed` is unset is pushed with no lease. A ref that motte maps to no git name is never acted on.
4. If the remote refuses the push as stale, the remote ref is not at `pushed`: someone moved it outside ward. ward then reconciles that ref. It reads the remote tip with `git ls-remote`, records it as `pushed`, and acts again at once with no wait. If the second push is refused as stale too, the failure is `remote_rejected` and the backoff applies.
5. On success, record `pushed` (the commit, or empty for a deletion) and `pushed_at`, clear `failure` and `attempts`, and clear `dirty` as State describes.

Every push is idempotent. Pushing a commit that the remote ref already holds changes nothing, and so does deleting a missing ref. A repeat of any step after a crash is therefore safe. This gives at-least-once pushes with no duplicate effect.

Ordering. The workers hold one lock for each `(mirror, ref)`, so two pushes of one ref never run together. Because step 1 reads the current ref, a later push never installs an older commit than an earlier push, whatever the order of the triggers. The order of the events of one ref is also fixed by motte: they share a partition and follow commit order. The mirror needs no order between two refs and assumes none. A worker takes the dirty refs of one repository together and sends them in one `git push` with one refspec for each ref. Git reports the result of each ref, and the worker records each result alone. The push is not atomic.

Read after write. A worker reads the ref from the metadata store of motte, so a read that follows an event shows that event or a later write. The route reads the row of the ref directly and answers 404 `vcs_not_found` when the row is absent. If a read shows a ref older than the event that made the trigger, the push is a repeat of an older push, and the later event, which motte also delivers, corrects it.

### Git access

The remote credential is a file or a GitHub App token (see GitHub App credential). ward reaches motte through git smart HTTP (see Git smart HTTP of motte) with a `read` grant. ward signs the grant with its own key: `repo` is the repository text, `scope` is `read`, and `exp` is 15 minutes after signing, so that one `git fetch` cannot outlive it. The verifier of motte checks only that `now < exp`, so it sets no cap on the life of a grant. The run of git has a 10 minute limit. The git routes of motte read the grant from `Authorization: Bearer` before they read the password of HTTP Basic, so ward sends the Bearer header on the first request and gets no 401 round trip. ward passes the header through the environment of the git process of motte only, never of the remote, (`GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_n` and `GIT_CONFIG_VALUE_n` set `http.extraHeader`). It never puts the grant, the remote credential or a URL with user information in an argument list, a file of the clone or a log line. The remote credential reaches git through `GIT_ASKPASS`. The helper is the `ward` binary, which git runs with the prompt as its only argument. The environment variables `WARD_ASKPASS_FILE` and `WARD_ASKPASS_USER` tell it to answer the user name or the content of the credential file, so it reads the file at each push.

The URL of the repository is `<WARD_MOTTE_URL>/git/<prefix>/<path>.git`, for the repository text `<prefix>:<path>`. The seed (see Seed) pushes to motte with a `merge` grant of the same life. For the calls to the control plane of motte (`/v1/events`, `/v1/subscriptions`) ward signs an `operator` grant with a life of 60 seconds. For `GET /v1/vcs/refs` it uses a `read` grant.

### Clone

ward keeps one bare clone for each repository text under `WARD_DATA_DIR/clones/`. The clone is full: ward never uses `--filter`, `--depth` or `--single-branch`. A push from a partial clone would need a promisor to fetch blobs while it packs, and the remote needs every object that the pushed commit reaches. motte offers no `shallow`, `deepen` or other filter than `blob:none`, and this agrees with that.

To get a commit, a worker fetches it by object name: `git fetch <motte URL> <commit>`. A `want` may name any object of the repository (see Git smart HTTP of motte), and a fetch by name needs no ref, so a ref that moves again cannot make it fail. The fetch speaks protocol v2, which motte requires (`Git-Protocol: version=2`). The worker then sets the local ref `refs/mirror/<git ref name>` to the commit, so that the objects stay reachable until the next push. After a deletion succeeds, the worker removes that local ref.

motte sends whole objects with no deltas, so a first fetch is larger than a repacked clone. The worker runs `git gc --auto` after each push batch.

If a git run fails in a way that is not a network, motte or remote error, such as a corrupt object, ward deletes the clone and acts again once. This is always safe, because the clone is a cache.

### Disk use and pruning

ward checks disk use only when a worker ends a push batch. It starts no timer for it.

- A clone that has no dirty ref and was not used for `WARD_CLONE_IDLE_TTL` is deleted. A worker marks a clone as used by setting the modification time of its directory. The next trigger fetches it again.
- When the cache uses more than `WARD_CLONE_DISK_BUDGET`, ward deletes idle clones, least recently used first, until it fits. A clone that a worker uses, or that has a dirty ref, is never deleted. If the budget is still exceeded, ward keeps working, reports `ward_mirror_disk_over_budget` and logs it.
- A clone of a repository that no configured mirror names is deleted.
- After a batch that deleted a ref, the worker runs `git gc --prune=now` on the clone, which frees the objects of the deleted ref.

### Failure and backoff

A failed action leaves the ref dirty, increments `attempts` and sets `next_attempt`, unless a trigger arrived since the worker read the ref: then `next_attempt` stays at the time of that trigger. The wait is 1 second, doubled after each attempt, up to 5 minutes, with random jitter of up to 20 percent. The cap holds until the action succeeds. A new trigger for the ref sets `next_attempt` to now, so fresh news is not held by an old backoff. One failing ref delays no other ref.

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

`GET /v1/mirrors` answers `{"mirrors":[...]}` with each mirror without its refs, and `GET /v1/mirrors/{name}` returns one with its refs. Both need an `operator` token. A `read` token reads `GET /v1/mirrors/{name}` only when the `repo` of the token equals the `repo` of the mirror, else 403 `forbidden`, also for a name that no mirror has. An `operator` token gets 404 `not_found` for an unknown name. The answer for a mirror:

```json
{
  "name": "...",
  "repo": "...",
  "state": "synced | pending | failing | seeding | seed_refused",
  "refs": [
    {"ref": "main", "state": "synced", "pushed": "<commit>", "pushed_at": "<RFC 3339>",
     "failure": "", "message": "", "attempts": 0, "failure_since": ""}
  ],
  "cursor": "<present only when more refs follow>"
}
```

A ref is `synced` when it is not dirty, `pending` when it is dirty with no failure, and `failing` when it has a failure class. The mirror takes the worst state of its refs. A mirror that has not finished its first Reconcile is `pending`. A mirror whose seed runs is `seeding`, and a mirror whose seed was refused is `seed_refused`; both replace the state of the refs. A refused seed adds `message`, the reason. A bad `cursor` answers 400 `invalid_body`. A list of refs has at most 1000 entries, with `?cursor=` as the opaque continuation.

`POST /v1/mirrors/{name}/reconcile` needs an `operator` token and answers 202. It makes the triggers of a Reconcile pass and returns at once.

### Observability

- Metrics, with names that start with `ward_`: `ward_mirror_pushes_total` (by mirror and result), `ward_mirror_dirty_refs` (by mirror), `ward_mirror_failing_refs` (by mirror and class), `ward_mirror_lag_seconds` (age of the oldest dirty ref, by mirror; the gauges of the state are read from the state store at each scrape), `ward_mirror_seeds_total` (by mirror and result: `seeded`, `refused` or `failed`), `ward_mirror_clone_bytes`, `ward_mirror_disk_over_budget`, `ward_hook_events_total` (by result) and `ward_http_requests_total` by route.
- Traces: OTLP, with the endpoint from the environment and no vendor in the code. Span names start with `ward.mirror.` for the work (`ward.mirror.fetch`, `ward.mirror.push`, `ward.mirror.seed`, `ward.mirror.seed.push`).
- Logs: JSON. A log line never holds a grant, a token, the hook secret, a remote credential, a remote URL or an event body.
- Instrumentation is additive. It never changes behavior or errors.

### Tests

The seed contract tests use a fresh motte repository and a remote with `main`, a branch and an annotated tag. They cover: a seed that copies every matching ref, the state `seeding` while the remote answers slowly, a mirror of a new motte commit after the seed, no second seed after a restart, a refusal when motte already has history, a restart of a half-done seed that writes nothing to the remote, a refusal when a caller moved a ref of motte between attempts, a fetch with a GitHub App token, and a long ref that `WARD_SEED_MAX_OBJECTS` splits into several pushes, and compressible files that `WARD_SEED_MAX_INFLATED_BYTES` splits.

The GitHub App contract tests use a fake token endpoint that checks the signature of the JWT with the public key and the claims, and a remote that accepts only the token that the endpoint issued last as the HTTP Basic password. They cover a push with a token, the cache across pushes, a refresh after the cached token nears its expiry, and a 401 from the endpoint. Contract tests start ward against a real motte (the `testserver` package of motte, which does not run the delivery worker) and a bare git repository that `git http-backend` serves over HTTP as the remote. A test reads the ref events from `GET /v1/events` and delivers them to the receiver with a real signature, as motte would, and it can drop an event to make a gap. They cover: a push, a force push, a delete, a ref that no pattern matches, a repeated delivery, a delivery with a bad signature, a delivery with a gap in its partition, a catch-up after downtime, a Reset after a 410, a push that the lease refuses because the remote moved, and a remote that rejects a ref. Waits use the injected clock: a test that checks the backoff replaces the clock and moves it, and the other tests wait for the report of a finished unit of work, never for time.

## Checks

A check proves one property of one tree, such as "the tests pass". ward records each result as a fact about content, keyed by the tree, so a land, a branch or a rebase that reaches the same tree reuses it. ward never runs a check. A runner runs it (see Runner).

Check file. The file `.ward/checks.json` at the tip of the target ref lists the required checks of that ref. The file is HuJSON (JWCC): JSON with `//` and `/* */` comments and trailing commas. ward standardizes it to JSON, and then reads it as strict JSON. A file that is not valid HuJSON fails each land onto that ref with `land_invalid`. A comment never hides text after the object. ward reads it from the tip, never from the change, so a change cannot remove or weaken the checks that judge it. A change to the file applies to the lands after it. A tip without the file has no required checks. A path `.ward` or `.ward/checks.json` that is not a tree or a regular file fails each land onto that ref with `land_invalid`. The format is `{"checks":[{"name","run","timeout_seconds"}]}`. `name` is 1 to 40 characters of `a-z 0-9 -` and is unique in the file. `run` is a non-empty list of strings: the argument vector that the runner runs at the root of the tree. `timeout_seconds` is 1 to 3600, and defaults to 1800. An unknown field, a file that does not parse, or a broken rule fails each land onto that ref with `land_invalid`, and the message names the rule. The hash of a check is the SHA-256 of its object `{"name","run","timeout_seconds"}` in that key order, with the default filled in and no spaces. The JSON text has no HTML escaping.

Result key. A result belongs to the key `(repo, tree, check hash, env)`. `tree` is the git tree ID. `env` is the `env` text of the runner of the repository (see Runner), such as an image digest, so a new environment proves each check again. A result is `pass` or `fail`, with a summary of at most 4096 bytes and an optional log URL of at most 2048 bytes. A pass is final: ward keeps it and never asks for that key again. A fail ends only the land that waited for it. A later land of the same tree asks for the check again. A fail ends a land only if it arrived after that land started to check that tree. When one key has both a pass and a fail, the check is flaky for that tree. ward keeps every result, and the status API of a land shows the flaky checks.

Runner. `WARD_RUNNERS` is a JSON list of `{repo, url, secret_file, env}`, one entry for each repository. An unknown field, a `url` that is not an `http` or `https` URL, a repeated `repo`, and a `secret_file` that is unreadable or empty refuse the start. `env` may be empty. ward reads `secret_file` for each request, so a rotated key applies with no restart. ward posts a check request to `url` with the JSON body `{request_id, repo, tree, commit, check, check_hash, env}`. `check` is the check object of the file. `commit` is a commit of motte whose tree is `tree`, so the runner can check it out. The header `Ward-Signature: sha256=<hex>` holds the HMAC-SHA256 of the body with the key of `secret_file`. An answer of 2xx accepts the request. Any other answer, or a network error, is retried with an exponential backoff from 1 second to 5 minutes. Each request to the runner has a time limit of 30 seconds. The backoff, the re-send deadline and that limit are the only timers of the checks. A request that has no result `timeout_seconds` plus 10 minutes after it was accepted is sent again with a new `request_id`. ward sends at most one request at a time for a key.

The runner answers with `POST /v1/results` and a `check` token of the repository. The body is `{request_id, status, summary, log_url}`. `status` is `pass` or `fail`. A body with an unknown field, another `status`, a `summary` of more than 4096 bytes or a `log_url` of more than 2048 bytes answers 400 `invalid_body`. An unknown `request_id` answers 404 `not_found`. A request of another repository answers 403 `forbidden`. A second result for one request answers 200 and changes nothing. A result wakes the land that waits for it, so no land polls.

### Local check

`ward check [--dir PATH] [--only NAME]` reads `.ward/checks.json` from the working tree at `PATH` (default `.`), with the parser of the service (`check.ParseFile`), and runs the `run` vector of each check in file order at `PATH`. It streams the output of each check, and then prints a line `pass` or `fail` for each check with its time. `--only` runs the one check with that name. It exits 0 when every check passed, 1 when a check failed or timed out, and 2 on a usage error, an unreadable or invalid check file, or an unknown `--only` name. Each check has its `timeout_seconds`.

The command is local and not authoritative. It never contacts a ward server and posts no result. A result counts for a land only when it is keyed by `(repo, tree, check hash, env)` and a runner with a `check` token posts it, so a pass on a workstation does not count for a land.

## Land

A land moves a motte branch onto a ref through the batched land of motte (see Land of motte). The ref is always green: it only ever holds a tree that passed every required check of the tip it replaced.

`POST /v1/lands` takes the JSON `{repo, ref, branch, delete_branch}`. `repo` and `branch` are required. `ref` is a stored ref name of motte and defaults to `main`. `delete_branch` defaults to false. The caller needs a `land` token (see Caller token). The `repo` of the body must equal the `repo` of the token, else 403 `forbidden`. A token of another scope answers 403 `forbidden`. A body with an unknown field answers 400 `invalid_body`. ward records the land in the state store and answers 202 with the land.

`GET /v1/lands/{id}` answers the land. The caller needs a `land` token of its repository, else 403 `forbidden`, and an unknown `id` answers 404 `not_found`. With `?wait=SECONDS`, at most 60, it answers when the state of the land changes or when the wait ends. A `wait` that is not a whole number of 0 to 60 answers 400 `invalid_body`. With `&state=STATE`, where `STATE` is a land state, the state of the land at the time of the request is compared with `STATE`: the answer comes at once when they differ, and else when the state changes or the wait ends. Without `state`, the state at the time of the request is the baseline. Another `state` text answers 400 `invalid_body`. The wait ends by the injected clock. The land is `{id, repo, ref, branch, state, tree, checks, tip, generation, fast_forward, commits, branch_deleted, error}`. `state` is `queued`, `checking`, `landing`, `landed` or `failed`. `checks` lists each required check with its name and its state: `pending`, `pass`, `fail` or `flaky`. `error` is set when the land failed: `{code, message, details}` with the code `land_conflict` (`details.paths`), `land_invalid`, `check_failed` (`details.check`, `details.summary`, `details.log_url`), `land_stale`, `land_timeout` or `internal_error`.

Queue. One worker for each `(repo, ref)` takes the lands of that ref one at a time, in the order of acceptance. The state store holds each land and its state, so a restart resumes every land that has not ended. A restart of a land in the state `landing` repeats the call of step 5 with the same values. If the state store cannot record a land, ward logs the error and stops the worker of that ref. The worker starts again at the next restart or at the next accepted land of that ref. Because the worker is the only caller that lands on the ref, another writer rarely moves it. The worker does these steps for one land:

1. Read the head of `branch` and the tip of `ref` with the commit API of motte. A missing branch or ref fails the land with `land_invalid`.
2. Find the base. The base is the commit that the branch was built on. ward reads commit objects with `GET /v1/vcs/objects/{sha}` and follows the first parent of both the tip and the head, one commit at a time on each side. The base is the first commit that one walk reaches and the other walk has already seen. A walk stops after 10000 commits on each side. If the chains never meet, the land fails with `land_invalid`.
3. Make the candidate. When the head descends from the tip, the candidate is the head. Otherwise ward writes the ref `refs/ward/candidates/<id>` at the tip and lands `base..head` onto it through `POST /v1/repos/{repo}/lands` of motte. The candidate is the tip of that land. motte makes the same tree for the same tip and change (see Land of motte), so the tree of the candidate is the tree that the ref will hold. A conflict fails the land with `land_conflict`. A range that motte refuses fails it with `land_invalid`.
4. Check. ward reads the check file at the tip and sets the state `checking`. It sends a request for each required check that has no pass for the tree of the candidate, and waits for the results. A fail ends the land with `check_failed`. When every check has a pass, the state is `landing`.
5. Land. ward calls `POST /v1/repos/{repo}/lands` of motte with `{id, ref, base, head, expect_tree}`, where `expect_tree` is the tree of the candidate and `id` is `<land id>-<attempt>`. `attempt` counts the times that the land reached step 5, from 1. ward stores `attempt`, `base`, `head` and `expect_tree` before the call, so a repeat after a crash carries the same `id` and values, and motte answers it with the first result. The call carries a `merge` grant that ward signs with its own key (see Service). The grant lives 5 minutes and has the subject `ward`. A `merge` grant lands on a protected ref, so a protected `main` is landable. A success ends the land as `landed` with the answer of motte. An answer 409 `vcs_land_stale` means that another writer moved the ref. ward goes back to step 1, at most 8 times, and then fails the land with `land_stale`. A failure of motte that does not end in time is tried again from step 1 in the same way. It counts against the same 8 times, and then the land fails with `land_timeout`. A failure that does not end in time is a `vcs_land_timeout`, or a call that passes its time limit. A read has a limit of 10 seconds, because a replica of motte can need several seconds for its first reads. The land call has the limit of the client, 35 seconds. A land call that passes its limit is repeated with the same `id` and values, because motte may have done it. An answer `vcs_land_conflict` fails the land with `land_conflict`. The answers `vcs_land_invalid`, `vcs_not_found` and `vcs_object_too_large` fail it with `land_invalid`. Any other failure of motte fails it with `internal_error`.
6. Clean up. ward deletes `refs/ward/candidates/<id>`. If `delete_branch` is true and the land succeeded, ward deletes the stored ref `branch` with the generation that it read in step 1. A delete that fails does not fail the land.

`ward land [--ref REF] [--delete-branch] REPO BRANCH` posts the land and then reads it with `wait=60` until it ends. It sends `wait=60` and `state=` with the last state that it saw. It reads `WARD_URL`, and a token from `WARD_TOKEN`, or else it signs a `land` token for `REPO` that lives 1 hour, with the Ed25519 key file `WARD_KEY_FILE` and the `kid` `WARD_KID`. Else it uses the token that `ward login` saved (see Human identity). With none of these it exits 2. It sends the token as `Authorization: Bearer`. It prints `checking <name>` once for each required check that has no pass when the state is first `checking`. On success it prints the lines `landed ...`, `tip <commit>`, `generation <n>`, `fast-forward` when it applies, one `<old> -> <new>` for each commit, and `branch deleted` when it applies, and it exits 0. On a failed land it prints the code and the message to standard error, the paths of a conflict, and the check, summary and log URL of `check_failed`, and exits 1. Any other failure exits 1. A usage error or a missing credential exits 2.

Contract tests land through a real motte and a fake runner that the test drives. They check: a land onto a ref without a check file; a land that waits for a pass and then lands the exact tree; a fail that ends the land and moves nothing; the reuse of a pass by a later land of the same tree; a fail that a later land of the same tree asks for again; a flaky check; a check file that a branch changes, which does not judge that branch; a ref that another writer moves during the checks, which makes the land check the new candidate; a conflict with its paths and an unmoved ref; a restart that resumes a land; the refusal of a token without the `land` or `check` scope or for another repository; `delete_branch`; and the exit codes of the command.

## Release

The image `ghcr.io/majorcontext/ward` is built from `images/ward/Dockerfile`: a static binary (`CGO_ENABLED=0`) on a `debian:bookworm-slim` base that is pinned by digest, with `git` and CA certificates, and it runs as the non-root user 65532. The mirror runs the `git` binary, and a distroless static base has none. The workflow `.github/workflows/image.yml` builds and pushes it for `linux/amd64` and `linux/arm64` on a tag `v*`, with the tag as the version in the binary.

The chart `charts/ward` runs one replica with the strategy `Recreate`, because the SQLite state store has a single writer. A PersistentVolumeClaim holds `WARD_DATA_DIR`. The chart mounts the motte signing key, the hook secret and the credential of each mirror (a token file, or a GitHub App key) as files from Kubernetes Secrets. `WARD_MIRRORS` comes from the values. Liveness uses `/health` and readiness uses `/ready`. The CI job `chart` lints the chart and renders it with each file of `charts/ward/ci/`.

## Planned work

Nothing in this section is built. Each paragraph names the intent and the motte mechanism that it uses.

### Change requests

A change is a stable change ID plus its current commit. An agent rewrites a change many times, so ward records each rewrite as a revision of the same change and keeps the history of revisions. The change ID comes from the `change-id` header that a commit carries and that a land keeps (see Land of motte). A change request is the proposal to land a change on a target ref. It uses motte refs and ref events: a change moves when its ref moves.

### Review threads and approvals

A review thread is a comment on a path and a line of a revision, with replies and a resolved flag. An approval names a revision and a reviewer. ward carries a thread to the next revision when the lines are unchanged. Approvals are policy of ward and use no motte mechanism.

### Merge queue

The queue orders approved change requests for a target ref. It builds on the queue of Land, which takes one land at a time. The merge queue lands a batch of changes together and tests the tree of the batch first. It lands through `POST /v1/repos/{repo}/lands` with a `merge` grant (see Land of motte), so the land uses the conflict check and the rebase of motte and needs no compare-and-swap loop in ward. When a batch fails, the queue bisects it: it splits the batch and tests each half until it finds the change that fails, and it removes that change and lands the rest. The target ref is a protected ref of motte, so only a `merge` grant moves it (see Protected refs of motte).

### CI orchestration

Checks (see Checks) are flat: each runs alone, and the runner runs them. A pipeline adds jobs with dependencies and artifacts that pass between them. It extends the check file and keeps the result key, so a job whose inputs did not change reuses its pass.

### Vanity Go import host

ward answers the `go-import` meta tag for a configured domain, so `go get` for a vanity import path resolves to the git smart HTTP URL of a motte repository (see Git smart HTTP of motte). The host serves the meta tag only, and the `go` tool fetches the code from motte with a credential that the caller supplies.

## Versioning

The path prefix `/v1` is additive. New fields and new routes are allowed, and clients ignore unknown fields. A breaking change gets `/v2`, served beside `/v1`. The module is tagged with semantic versions.
