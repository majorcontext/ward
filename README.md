# ward

ward is the forge layer on top of [motte](https://github.com/majorcontext/motte): change requests, review, the merge queue, CI orchestration and mirroring to external git hosts. motte holds the mechanism (objects, refs, ref events, git smart HTTP, land). ward holds the policy and the workflow.

Status: early. The repository holds milestone W1: the specification and the repository tooling. The mirror is the first feature and is specified in full. Everything else in `docs/architecture.md` that is marked Planned is intent only. No feature code exists yet.

Read `docs/architecture.md` for the design. Read `AGENTS.md` before you change the repository.

License: MIT.
