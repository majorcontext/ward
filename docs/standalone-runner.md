# Running `ward runner`

`ward runner` receives check requests from ward, fetches the exact tree from motte, runs each check in a sandbox and posts the result. The contract is in `docs/architecture.md`, Standalone runner.

## What you need

- ward, and a `WARD_RUNNERS` entry for each repository: `{"repo":"native:widgets","url":"http://runner.internal:8081/v1/checks","secret_file":"/etc/ward/runner-hook-secret","env":"IMAGE-DIGEST"}`. Use the digest of the check image as `env`, so a new image proves each check again.
- The same secret in the file of `WARD_RUNNER_SECRET_FILE` on the runner.
- A key pair for check tokens. Put the public key in `WARD_TRUSTED_KEYS` of ward with a `kid` and the scope `check`. Give the private key (base64) to the runner with `WARD_RUNNER_KID` and `WARD_RUNNER_KEY_FILE`.
- A `read` grant of motte for the repositories, in the file of `WARD_RUNNER_MOTTE_GRANT_FILE`. Replace the file before the grant expires: the runner reads it for each fetch.
- A container engine (`docker` or `podman`), and an OCI image in which the checks run. The image holds the tools that the checks call. The default network is `none`, so the image must hold what a check needs, or you set `WARD_RUNNER_NETWORK`.
- For gVisor, `runsc` installed and registered as a runtime of the engine. With `WARD_RUNNER_SANDBOX=auto` the runner uses it when `runsc` is on the path. Set `gvisor` to refuse to start without it.

## A Linux machine

1. Install the engine, and `runsc` if you want gVisor. Install the `ward` binary.
2. Create the user `ward-runner` that may use the engine.
3. Write the three secret files with mode 0600, owned by that user.
4. Write `/etc/systemd/system/ward-runner.service`:

```ini
[Unit]
Description=ward check runner
After=network-online.target

[Service]
User=ward-runner
Environment=WARD_RUNNER_LISTEN=:8081
Environment=WARD_RUNNER_WARD_URL=https://ward.example.org
Environment=WARD_RUNNER_SECRET_FILE=/etc/ward-runner/hook-secret
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

5. Run `systemctl enable --now ward-runner`. `GET /health` answers 200. Put the listener behind TLS when ward reaches it over a network that you do not control. The signature authenticates ward to the runner, and the check token authenticates the runner to ward.

## Kubernetes

Set `runner.enabled=true` in the chart `charts/ward`, and fill `runner.image`, `runner.hookSecret`, `runner.checkKey`, `runner.motte` and `runner.sandboxImage` (see `charts/ward/ci/runner.yaml`). The chart creates a Deployment and a Service `RELEASE-runner`. Build the runner image from `images/ward-runner/Dockerfile`: it holds `ward` and podman.

A container engine in a pod needs rights. Either set `runner.privileged=true`, or give the pod an engine through `runner.extraVolumes`, `runner.extraVolumeMounts` and `runner.extraEnv`. A node engine socket resolves the tree directory on the node, so a path of the pod must also exist on the node. Isolate the runner pod: it holds a motte read grant, and the checks of a repository are code that its authors wrote.

## Writing another runner

Read `runnerproto/runner.schema.json`. A Go runner imports `runnerproto`. A runner in another language must produce the same bytes for the signature. Run `runnerproto/runnertest.Run` against your handler to prove the contract.
