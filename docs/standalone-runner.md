# Running `ward runner`

`ward runner` receives check requests from ward, fetches the exact tree from motte, runs each check in a sandbox and posts the result. The contract is in `docs/architecture.md`, Standalone runner.

## How a runner gets work

The runner polls ward (`POST /v1/runner/leases`), so ward never connects to it. It needs outbound HTTPS to ward and to motte, and nothing inbound. The lease ends when the runner stops renewing it, and ward then leases the same request again. Behind a tunnel, expose `POST /v1/runner/leases`, `POST /v1/runner/leases/{lease_id}/renew` and `POST /v1/results`. A poll lasts up to 20 seconds.

A `WARD_RUNNERS` entry: `{"repo":"native:widgets","kids":["metal-1","metal-2"],"env":"IMAGE-DIGEST"}`. The default `min_isolation` is `gvisor`.

## Identity and isolation

Each pull runner has its own Ed25519 key. Put the public key in `WARD_TRUSTED_KEYS` with the scopes `runner` and `check`, and the `kid` in the `kids` of each repository that the runner may serve. A runner gets only requests of those repositories. ward rejects a result from a runner that is not in `kids` of the repository, or that did not lease the request. To revoke one runner, remove its key or its `kid`: no other runner changes.

The runner reports its sandbox in each poll. ward refuses a runner whose sandbox is weaker than `min_isolation` of every repository that it serves. A repository allows `container` only when its entry says `"min_isolation":"container"`. There is no global switch that lowers it.

## What you need

- ward, and a `WARD_RUNNERS` entry for each repository, as above. Use the digest of the check image as `env`, so a new image proves each check again.
- A key pair for check tokens. Put the public key in `WARD_TRUSTED_KEYS` of ward with a `kid` and the scope `check`. Give the private key (base64) to the runner with `WARD_RUNNER_KID` and `WARD_RUNNER_KEY_FILE`.
- A `read` grant of motte for the repositories, in the file of `WARD_RUNNER_MOTTE_GRANT_FILE`. Replace the file before the grant expires: the runner reads it for each fetch.
- A container engine (`docker` or `podman`), and an OCI image in which the checks run. The image holds the tools that the checks call. The default network is `none`, so the image must hold what a check needs, or you set `WARD_RUNNER_NETWORK`.
- For gVisor, `runsc` installed and registered as a runtime of the engine. With `WARD_RUNNER_SANDBOX=auto` the runner uses it when `runsc` is on the path. Set `gvisor` to refuse to start without it.

## The host contract

A runner lives on any Linux host that provides these. Provisioning stays outside ward: use any provider, a module of your own, cloud-init, or a machine that you built by hand.

- A container engine (`docker` or `podman`). With rootless `podman` the runner passes `--userns keep-id`, so the check runs as the uid of the runner and can write its work and cache directories.
- `runsc` registered as a runtime of the engine, and `/dev/kvm` usable by the service user.
- A cache directory and a work directory on local disk. Not a network file system, not `tmpfs`.
- Outbound HTTPS to ward and to motte.
- A service user, the `ward` binary, a motte `read` grant file.

## Install on a Linux machine

```
ward runner install --ward-url https://ward.example.org --kid metal-1 \
  --motte-url https://motte.example.org --motte-grant-file /etc/ward-runner/motte-grant \
  --image registry.example.org/check-env@sha256:...
```

The command checks the host, writes `/etc/systemd/system/ward-runner.service` and `/etc/ward-runner/runner.env`, creates the check key, the cache directory and the work directory when they are absent, and prints the `WARD_TRUSTED_KEYS` entry. It prints each missing prerequisite and exits 1. Fix those, add the key and the `kid` to ward, and run `systemctl daemon-reload && systemctl enable --now ward-runner`.

## Cache

With `WARD_RUNNER_CACHE_DIR` the runner keeps one directory for each repository and env across runs. The check sees it at `/cache`, read-write, and `GOCACHE`, `GOMODCACHE` and `XDG_CACHE_HOME` point into it. The sandbox of a repository mounts only its own directory, so one repository cannot read or change the cache of another.

## A Linux machine by hand

1. Install the engine, and `runsc` if you want gVisor. Install the `ward` binary.
2. Create the user `ward-runner` that may use the engine.
3. Write the two secret files with mode 0600, owned by that user.
4. Write `/etc/systemd/system/ward-runner.service`:

```ini
[Unit]
Description=ward check runner
After=network-online.target

[Service]
User=ward-runner
Environment=WARD_RUNNER_WARD_URL=https://ward.example.org
Environment=WARD_RUNNER_KID=runner-1
Environment=WARD_RUNNER_KEY_FILE=/etc/ward-runner/check-key
Environment=WARD_RUNNER_MOTTE_URL=https://motte.example.org
Environment=WARD_RUNNER_MOTTE_GRANT_FILE=/etc/ward-runner/motte-grant
Environment=WARD_RUNNER_SANDBOX=auto
Environment=WARD_RUNNER_ENGINE=podman
Environment=WARD_RUNNER_IMAGE=registry.example.org/check-env@sha256:...
Environment=WARD_RUNNER_CONCURRENCY=2
Environment=WARD_RUNNER_WORK_DIR=/var/lib/ward-runner
StateDirectory=ward-runner
ExecStart=/usr/local/bin/ward runner
Restart=always

[Install]
WantedBy=multi-user.target
```

5. Run `systemctl enable --now ward-runner`.

## Kubernetes

Set `runner.enabled=true` in the chart `charts/ward`, and fill `runner.image`, `runner.checkKey`, `runner.motte` and `runner.sandboxImage` (see `charts/ward/ci/runner.yaml`). The chart creates a Deployment `RELEASE-runner` and no Service, because the runner polls ward. Put `runner.checkKey.kid` in the `kids` of an entry of `runners`. Build the runner image from `images/ward-runner/Dockerfile`: it holds `ward` and podman.

A container engine in a pod needs rights. Either set `runner.privileged=true`, or give the pod an engine through `runner.extraVolumes`, `runner.extraVolumeMounts` and `runner.extraEnv`. A node engine socket resolves the tree directory on the node, so a path of the pod must also exist on the node. Isolate the runner pod: it holds a motte read grant, and the checks of a repository are code that its authors wrote.

## Writing another runner

Read `runnerproto/runner.schema.json`. A Go runner imports `runnerproto`. A runner in another language polls and renews leases and posts results as the schema says.
