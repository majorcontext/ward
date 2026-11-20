# ward

ward is the forge layer on top of [motte](https://github.com/majorcontext/motte): change requests, review, the merge queue, CI orchestration and mirroring to external git hosts. motte holds the mechanism (objects, refs, ref events, git smart HTTP, land). ward holds the policy and the workflow.

Status: early. The repository holds the specification, the mirror (including a GitHub App credential), the always-green land of a branch through motte (checks that a runner proves, a queue for each ref), and the image and Helm chart. Everything else in `docs/architecture.md` that is marked Planned is intent only.

Read `docs/architecture.md` for the design. Read `AGENTS.md` before you change the repository.

License: MIT.

## Developing ward

ward lands its own changes through ward. The file `.ward/checks.json` lists the required checks: `build-vet`, `lint`, `test` and `gofmt`. The GitHub jobs `chart` and `leak` stay outside it: `chart` needs `helm`, which a check environment does not guarantee, and `leak` needs the secret `LEAK_DENYLIST`, which a check must never receive.

The local loop:

1. Clone the repository and make a branch.
2. Run `ward check` at the root. It runs every check of `.ward/checks.json` in the working tree and prints a pass or fail summary with the time of each check. `ward check --only NAME` runs one. The run is local. It never contacts a ward server and posts no result, so a pass here does not count for a land.
3. Push the branch.
4. Run `ward land REPO BRANCH` (see `docs/architecture.md`, Land). The runner of the repository proves each check on the exact tree, and the land moves `main` only when every check passed.

A box that runs the same commands is an optional place to work. The local loop needs no box.
