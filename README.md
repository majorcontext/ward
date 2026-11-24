# ward

ward is the forge layer on top of [motte](https://github.com/majorcontext/motte): change requests, review, the merge queue, CI orchestration and mirroring to external git hosts. motte holds the mechanism (objects, refs, ref events, git smart HTTP, land). ward holds the policy and the workflow.

Status: early. The repository holds the specification, the mirror (including a GitHub App credential), the always-green land of a branch through motte (checks that a runner proves, a queue for each ref), and the image and Helm chart. Everything else in `docs/architecture.md` that is marked Planned is intent only.

Read `docs/architecture.md` for the design. Read `AGENTS.md` before you change the repository.

License: MIT.

## Running checks

ward never runs a check. A runner does, and ward needs one runner for each repository that has a `.ward/checks.json`. Two kinds work, and both speak the protocol of the Go package `runnerproto` (JSON schema: `runnerproto/runner.schema.json`).

| | External runner | `ward runner` |
|---|---|---|
| What it is | A service that you already have, or write: it receives a signed check request and posts a result. | A process of this repository. It fetches the exact tree from motte and runs each check in a sandbox. |
| Setup | An entry in `WARD_RUNNERS` (`repo`, `url`, `secret_file`, `env`). | The same entry, with the `url` of the runner, plus the environment of `ward runner`. Or a pull entry (`mode: pull`, `kids`) and `ward runner install`. |
| Sandbox | Its own. | A container through any OCI engine (docker, podman), under gVisor (`runsc`) when it is on the path. |
| Needs | Whatever the service needs. | A container engine, a read grant of motte and a check key that ward trusts. |

A runner receives its work in one of two ways. In push mode ward posts each request to the URL of the runner, so ward must reach the runner. In pull mode the runner dials out to ward, long-polls for a lease on a request, renews the lease while it runs, and posts the result with its own check token. Pull needs no inbound network, so a runner can sit behind a firewall or a tunnel. Each pull runner has its own key (`kid`), listed in the `kids` of each repository that it may serve. A pull runner must run its checks under gVisor unless the entry of the repository says `"min_isolation":"container"`.

A runner can live on any bare-metal Linux host of any provider, or on your own hardware. ward holds no provider code. The host needs a container engine, `runsc` with `/dev/kvm`, a cache directory on local disk and outbound HTTPS. `ward runner install` checks the host, writes the systemd unit and the environment file, and prints what is missing.

No specific vendor is required for either. A runner by another author can test itself with `runnerproto/runnertest`. See `docs/standalone-runner.md` to run `ward runner` on Kubernetes (the chart value `runner.enabled`) or on a Linux machine. The wire and the variables are in `docs/architecture.md`, Runner protocol and Standalone runner.

## Developing ward

ward lands its own changes through ward. The file `.ward/checks.json` lists the required checks: `build-vet`, `lint`, `test` and `gofmt`. The GitHub jobs `chart` and `leak` stay outside it: `chart` needs `helm`, which a check environment does not guarantee, and `leak` needs the secret `LEAK_DENYLIST`, which a check must never receive.

The local loop:

1. Clone the repository and create a branch.
2. Run `ward check` at the root. It runs every check of `.ward/checks.json` in the working tree and prints a pass or fail summary with the time of each check. `ward check --only NAME` runs one. The run is local. It never contacts a ward server and posts no result, so a pass here does not count for a land.
3. Push the branch.
4. Run `ward land REPO BRANCH` (see `docs/architecture.md`, Land). The runner of the repository proves each check on the exact tree, and the land moves `main` only when every check passed.

A person needs no identity provider, no tunnel and no box. `WARD_URL` and `MOTTE_URL` are the addresses of the deployments, and `REPO` is the repository path. An operator who holds an operator token (`WARD_TOKEN`, or `WARD_KEY_FILE` and `WARD_KID` of a trusted key) issues a token for you:

```
WARD_URL=$WARD_URL ward token create --principal you@example.test --repo native:REPO
```

The command prints the token once. `ward land` sends it as a bearer token. Git talks to motte, which holds its own per-user tokens: the same operator issues one with `motte token create` (see motte), and Git sends it as the password.

```
git clone https://you:MOTTE_TOKEN@MOTTE_HOST/git/REPO.git && cd REPO && git switch -c my-change
ward check
git push origin my-change
WARD_URL=$WARD_URL WARD_TOKEN=WARD_TOKEN ward land REPO my-change
```

`ward token list` shows the tokens and `ward token revoke ID` revokes one. `git push` writes a branch and never a protected `main`: only `ward land` lands a branch.

A deployment that trusts an OIDC issuer (`WARD_OIDC_*`) lets you sign in without a token: `WARD_OIDC_ISSUER=ISSUER WARD_OIDC_CLIENT_ID=ID ward login` runs the OAuth device flow and saves the token with mode 0600, and `ward land` uses it. An identity-aware proxy that signs a JWT in its own header, such as Cloudflare Access, is one configuration of the same verifier (see `docs/architecture.md`, Human identity). Behind such a proxy, `ward land` sends `WARD_ACCESS_TOKEN` in the header `WARD_ACCESS_HEADER` (default `cf-access-token`) and needs no other credential. ward serves TLS itself when `WARD_TLS_CERT` and `WARD_TLS_KEY` are set.

A box that runs the same commands is an optional place to work. The local loop needs no box.
