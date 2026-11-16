# Agents

processconcierge supervises child processes so nothing they start outlives them. Humans read [README.md](README.md).

## Layout

- `pkg/proc`: start a child in a unit (a process group on macOS and Linux, a kill-on-close Job Object on Windows) and stop the whole unit, gently then for good.
- `pkg/sweep`: the registry folder of supervised children and the start-up sweep.
- `pkg/slots`: slots shared across programs through a folder of OS file locks, held back under memory pressure.
- `pkg/pressure`: each platform's memory pressure signal, read once or watched.
- `pkg/procerr`: the typed errors every package returns, each with a stable `Code`.
- `internal/reaper`: one tini-style run for a parent that cannot supervise processes itself.
- `internal/proctest`: what the process tests share: helper processes the test binary turns into, and the waits and reads.
- `internal/cli` and `cmd/processconcierge`: the Cobra command (`run`, `sweep`, `version`); `main` only calls `cli.Execute`.
- MUST NOT add loose `.go` files at the module root, or grab-bag packages such as `util` or `common`.

## Rules

- Before writing Go here, load `octo-go:golang-quality`; before changing the command, also `octo-go:cli-command-surface`.
- Each platform's code lives in its own file with a build tag or a `_linux`, `_windows` suffix. A change to one platform MUST keep the others building: `go tool task vet` vets this system, Linux and Windows.
- A child's unit is the only thing ever signalled. MUST NOT signal a bare pid that might have been reused; the sweep checks the start time first.
- Supervision retries a start only before any process exists, and only for errors that pass (`transientStart`); a start that fails once the child exists is never retried, so one start never leaves two children. Starting a child again after it ran is the owner's decision.
- Libraries emit spans on the global tracer provider and never install one; only `internal/cli` sets up olly.
- Run `go tool task format lint vet test smoke` before calling a change done. The tests start real processes; CI runs them on macOS, Linux and Windows, and Windows code is only proven there.
- Releases: once CI passes on a merge to `main`, `auto-patch-release.yml` tags the next patch and publishes binaries for six platforms with GoReleaser. Bump minor or major by running that workflow by hand.
