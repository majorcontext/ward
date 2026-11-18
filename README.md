# ward

ward is the forge layer on top of [motte](https://github.com/majorcontext/motte): change requests, review, the merge queue, CI orchestration and mirroring to external git hosts. motte holds the mechanism (objects, refs, ref events, git smart HTTP, land). ward holds the policy and the workflow.

Status: early. The repository holds the specification, the mirror (including a GitHub App credential), the land of a branch through motte, and the image and Helm chart. Everything else in `docs/architecture.md` that is marked Planned is intent only.

Read `docs/architecture.md` for the design. Read `AGENTS.md` before you change the repository.

License: MIT.
