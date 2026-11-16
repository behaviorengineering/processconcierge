# processconcierge

processconcierge supervises child processes so that nothing they start outlives them. It does for a desktop app what [tini](https://github.com/krallin/tini) does inside a container: it sits between a parent and a child, keeps the child and everything the child starts in one unit, and stops that whole unit when the child is done, when it is asked to, or when the parent dies, however the parent dies.

It started with [Totem](https://github.com/digizoo/digizoo.ai.totem), whose Claude Code chats each keep a Claude Code process alive, with its shells and MCP servers under it. A crash or a force-quit of Totem's backend must not leave those running.

## Why not an existing supervisor

The well-known supervisors each miss one part of this job.

- **tini and dumb-init** rely on being PID 1 in a container, or on Linux's subreaper, so they do not run on macOS or Windows.
- **supervisord, PM2, process-compose and s6** keep a fixed set of services alive and take over each child's stdin and stdout. A parent that talks to its child over stdin and stdout, as the Claude Agent SDK does, needs those passed straight through.
- **A supervisor inside the parent** dies with the parent, so it cannot clean up after a crash.

processconcierge is built from the operating system's own tools for this: process groups on macOS and Linux, a parent-death signal on Linux, and Job Objects on Windows.

## What each platform does

| | Holds the child and what it starts | When the parent dies |
| --- | --- | --- |
| macOS | A process group | The reaper sees its owner's pipe close and stops the group |
| Linux | A process group | The same, and the kernel's parent-death signal stops the child even if the reaper is killed outright |
| Windows | Not yet: a kill-on-close Job Object is next | Not yet |

A child that moves itself into a new process group escapes a group stop on macOS and Linux. The registry and its sweep catch it at the next start.

## The reaper, for a parent that is not Go

Run the child under `processconcierge run`, and hand it the read end of a pipe as file descriptor 3. The parent never writes to the pipe. Every way the parent can end, a SIGKILL included, closes it, and the reaper then stops the child's whole group.

From Node:

```js
import { spawn } from 'child_process';

const child = spawn('processconcierge', [
  'run', '--owner-fd', '3', '--registry', registryDir, '--',
  'claude', ...args,
], { stdio: ['pipe', 'pipe', 'pipe', 'pipe'] });
// child.stdin and child.stdout are the program's own: the reaper passes them through untouched.
```

The reaper:

- runs the program in a process group of its own, with the reaper's stdin, stdout and stderr;
- stops the group when the program exits, when the reaper gets SIGTERM, SIGINT or SIGHUP, or when the owner's pipe closes: SIGTERM first, then SIGKILL for whatever is left after `--grace` (5 seconds by default);
- exits with the program's code, or 128 plus the signal's number when a signal ended it, and 127 when the program could not start;
- records the program in the `--registry` folder while it runs, so a later sweep can find it.

At start-up, before starting any children, the parent runs:

```sh
processconcierge sweep --registry "$registryDir"
```

It kills the group of every recorded program that is still running, or whose group still holds something the program started, as on Linux, where the parent-death signal ends the program but not its descendants. It passes over any entry whose pid now belongs to another program, which it can tell from the start time the entry records. `--dry-run` reports the same and changes nothing; `--json` prints the results for a program to read.

## From Go

```go
p, err := proc.Config{Path: "claude", Args: args, Stdin: in, Stdout: out}.Start(ctx)
if err != nil {
    return err
}
defer p.Stop(ctx) // SIGTERM to the group, the grace period, then SIGKILL for what is left

reg := sweep.Registry{Dir: registryDir}
if err := reg.Record(ctx, p.Pid()); err != nil {
    return err
}
defer reg.Remove(p.Pid())
```

Every error is a `*procerr.Error` with a stable `Code`, so callers can match `procerr.Is(err, procerr.CodeStart)` and still reach the cause.

## Coming next

- **Windows:** each child in a kill-on-close Job Object, created before the child runs.
- **Slots:** crash-safe slot files, held with OS file locks, that programs on the same machine share, so they never start more children between them than the machine can hold.
- **Memory pressure:** a watch on each platform's own signal, which stops new starts and tells the parent, which decides what to close.
- **Tracing:** spans for each start, stop and sweep, through [olly](https://github.com/behaviorengineering/olly).

## Working on it

```sh
go tool task          # the list of tasks
go tool task test     # starts real processes; runs on macOS and Linux
go tool task vet      # vets this system, Linux and Windows
```
