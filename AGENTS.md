# Agents

processconcierge supervises child processes so nothing they start outlives them. Humans read [README.md](README.md).

## Layout

- `pkg/proc`: start a child in a unit (a process group on macOS and Linux, a Job Object on Windows once supported) and stop the whole unit, gently then for good.
- `pkg/sweep`: the registry folder of supervised children and the start-up sweep.
- `pkg/procerr`: the typed errors every package returns, each with a stable `Code`.
- `internal/reaper`: one tini-style run for a parent that cannot supervise processes itself.
- `internal/cli` and `cmd/processconcierge`: the Cobra command (`run`, `sweep`, `version`); `main` only calls `cli.Execute`.
- MUST NOT add loose `.go` files at the module root, or grab-bag packages such as `util` or `common`.

## Rules

- Before writing Go here, load `octo-go:golang-quality`; before changing the command, also `octo-go:cli-command-surface`.
- Each platform's code lives in its own file with a build tag or a `_linux`, `_windows` suffix. A change to one platform MUST keep the others building: `go tool task vet` vets this system, Linux and Windows.
- A child's unit is the only thing ever signalled. MUST NOT signal a bare pid that might have been reused; the sweep checks the start time first.
- Supervision never retries: starting a long-lived child again is the owner's decision, not this library's.
- Run `go tool task format lint vet test smoke` before calling a change done. The tests start real processes, so they run on macOS and Linux.
