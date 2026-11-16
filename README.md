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
| macOS | A process group | The reaper sees its owner go and stops the group |
| Linux | A process group | The same, and the kernel's parent-death signal stops the child even if the reaper is killed outright |
| Windows | A Job Object that kills every process in it when its last handle closes, joined before the child runs | The reaper sees its owner go and stops the job; and if the reaper itself is killed, Windows ends the job |

A child that moves itself into a new process group escapes a group stop on macOS and Linux. The registry and its sweep catch it at the next start. On Windows nothing escapes: a job allows no breakaway.

## Install

Download the archive for your platform from the [releases](https://github.com/behaviorengineering/processconcierge/releases): macOS, Linux and Windows, each on amd64 and arm64, with a `checksums.txt`. An app such as Totem ships the binary for each platform it runs on.

`go install github.com/behaviorengineering/processconcierge/cmd/processconcierge@latest` also works; its `version` reports the module version.

## The reaper, for a parent that is not Go

Run the child under `processconcierge run`, and tell the reaper how to notice its owner going:

- **`--owner-fd 3`** on macOS and Linux: hand the reaper the read end of a pipe as file descriptor 3. The parent never writes to it, and every way the parent can end, a SIGKILL included, closes it.
- **`--owner-pid <pid>`** on Windows, where Node passes extra pipes in a way Go does not read: the reaper waits on a handle to the owner's process, which a reused pid cannot fool. It also works on macOS and Linux, where the reaper looks every 200 ms. The reaper refuses `--owner-fd` on Windows.

From Node, on macOS and Linux:

```js
import { spawn } from 'child_process';

const child = spawn('processconcierge', [
  'run', '--owner-fd', '3', '--registry', registryDir, '--',
  'claude', ...args,
], { stdio: ['pipe', 'pipe', 'pipe', 'pipe'] });
// child.stdin and child.stdout are the program's own: the reaper passes them through untouched.
```

On Windows, pass `'--owner-pid', String(process.pid)` instead of `'--owner-fd', '3'`, with the usual three stdio pipes.

The reaper:

- runs the program in a unit of its own (a process group, or a Job Object on Windows), with the reaper's stdin, stdout and stderr;
- stops the unit when the program exits, when the reaper gets SIGTERM, SIGINT or SIGHUP, or when its owner goes: SIGTERM first (CTRL_BREAK on Windows, where the program shares a console), then a hard stop for whatever is left after `--grace` (5 seconds by default), leftovers included. A second stop request during that skips the rest of the grace period;
- watches for all of these from the moment it starts, so a stop request while it waits for a slot means the program never starts;
- leaves a program that is stuck inside a kernel call after the hard stop to die by itself: such a program can run none of its own code, and dies when the call returns, so the reaper reports it, keeps its registry entry for the next sweep, and exits rather than wait on it;
- records the program in the `--registry` folder while it runs, so a later sweep can find it;
- with `--slots`, takes a slot before starting the program, as below.

It exits with the program's code, or 128 plus the signal's number when a signal ended it, and otherwise with one of its own:

| Code | Meaning |
| --- | --- |
| 70 | The program is stuck in the kernel after the hard stop; its registry entry is kept |
| 75 | No slot came free within `--slot-wait`; the program was not started |
| 127 | The program could not be started |
| 143 | A stop request arrived before the program started, so it never did |

At start-up, before starting any children, the parent runs:

```sh
processconcierge sweep --registry "$registryDir"
```

It goes through every entry, even when one fails: an entry whose group it could not kill stays for the next sweep, and the results are printed either way. It kills the group of every recorded program that is still running, or whose group still holds something the program started, as on Linux, where the parent-death signal ends the program but not its descendants. It passes over any entry whose pid now belongs to another program, which it can tell from the start time the entry records. `--dry-run` reports the same and changes nothing; `--json` prints the results for a program to read.

## Slots and memory pressure

Several programs on one machine can share a limit on how many children run at once. Each passes the same folder and count:

```sh
processconcierge run --slots ~/.cache/myapp/slots --slot-count 8 --slot-wait 30s -- claude
```

Each slot is a file held with an operating-system file lock (flock on macOS and Linux, LockFileEx on Windows). The system lets go of a lock when its holder ends, however it ends, so a crash never leaks a slot and nothing needs cleaning up. While every slot is held, the reaper waits up to `--slot-wait`, then exits 75 without starting the program.

Slots are also held back while the machine is short of memory, at `--hold-at critical` by default (or `warning`). The reading comes from each platform's own signal:

| | Signal |
| --- | --- |
| macOS | The kernel's `kern.memorystatus_vm_pressure_level` |
| Linux | Pressure stall information in `/proc/pressure/memory`, or available memory on kernels without it |
| Windows | The system's low-memory notification, then the share of memory still available |

`processconcierge pressure` shows the reading, and `processconcierge slots --dir <folder>` shows how many slots are free; both take `--json`.

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

`Start` tries a start again, up to three more times within about two seconds, only when the system refused it for a reason that passes (too many processes or open files, the program file busy because it is being replaced, Windows antivirus holding it) and no process was created. A start that fails once the child exists is never tried again, so one start never leaves two children. `Stop` returns an error with `CodeStuck` for a child stuck in the kernel after the hard stop.

On macOS, a Go parent that can be killed outright sets `Reaper` to the path of the processconcierge command, so its children run under the reaper and never outlive it:

```go
p, err := proc.Config{Path: "claude", Args: args, Reaper: "/path/to/processconcierge"}.Start(ctx)
```

On Linux the parent-death signal already ends the child (though not its grandchildren, which `Reaper` covers too), and on Windows the Job Object ends with the parent, so `Reaper` is optional on Linux and ignored on Windows.

Slots and pressure work the same way from Go:

```go
slot, err := slots.Config{Dir: dir, Count: 8, Pressure: pressure.Read}.Take(ctx)
if err != nil {
    return err
}
defer slot.Release()

for reading := range pressure.Watch(ctx, 2*time.Second) {
    // Sent first, then on every change of level: close idle children at Warning or Critical.
}
```

Every error is a `*procerr.Error` with a stable `Code`, so callers can match `procerr.Is(err, procerr.CodeStart)` and still reach the cause.

## Tracing

Each start, stop, sweep, slot taken and change of memory pressure is an OpenTelemetry span on the global tracer provider. The library never installs one; the app does, with [olly](https://github.com/behaviorengineering/olly) or its own. The `processconcierge` command sends its spans when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. A stop's span says whether the hard stop was needed (`process.hard_stop`), so a trace shows which programs ignore SIGTERM.

## Working on it

```sh
go tool task          # the list of tasks
go tool task test     # starts real processes, on macOS, Linux and Windows
go tool task vet      # vets this system, Linux and Windows
```
