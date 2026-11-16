// Package cli is the processconcierge command: the reaper (run), the start-up sweep (sweep) and version. main only
// calls Execute.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/behaviorengineering/olly/pkg/olly"

	"github.com/behaviorengineering/processconcierge/internal/reaper"
	"github.com/behaviorengineering/processconcierge/pkg/pressure"
	"github.com/behaviorengineering/processconcierge/pkg/proc"
	"github.com/behaviorengineering/processconcierge/pkg/slots"
	"github.com/behaviorengineering/processconcierge/pkg/sweep"
)

// Exit codes the command uses for itself; run otherwise exits with its child's code.
const (
	ExitOK       = 0
	ExitInternal = 1
	ExitUsage    = 2
)

// Execute runs the command for args and returns the exit code.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer, version string) int {
	shutdown := startTracing(stderr)
	defer shutdown()
	code := ExitOK
	root := newRoot(stdout, version, &code)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		writeln(stderr, "processconcierge: "+err.Error())
		if code == ExitOK {
			writeln(stderr, "Run 'processconcierge help' for the commands.")
			return ExitUsage
		}
	}
	return code
}

func newRoot(stdout io.Writer, version string, code *int) *cobra.Command {
	root := &cobra.Command{
		Use:           "processconcierge",
		Short:         "Supervise child processes so nothing they start outlives them",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(*cobra.Command, []string) error {
			printGuide(stdout, version)
			return nil
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(
		&cobra.Command{
			Use:   "version",
			Short: "Print the release identity",
			Args:  cobra.NoArgs,
			RunE: func(*cobra.Command, []string) error {
				writeln(stdout, "processconcierge "+version)
				return nil
			},
		},
		newRun(code),
		newSweep(stdout, code),
		newPressure(stdout, code),
		newSlots(stdout, code),
	)
	return root
}

func newRun(code *int) *cobra.Command {
	var ownerFD, ownerPID, slotCount int
	var grace, slotWait time.Duration
	var registry, logFile, slotDir, holdAt string
	cmd := &cobra.Command{
		Use:   "run [flags] -- <program> [args...]",
		Short: "Run a program as a reaper would: in its own unit, stopped with everything it started",
		Long: "Runs the program with this command's own stdin, stdout and stderr, in a process group of its own. When the\n" +
			"program exits, when this command gets SIGTERM, SIGINT or SIGHUP, or when the owner's pipe closes, it sends\n" +
			"the group SIGTERM, waits the grace period, then kills whatever is left, and exits with the program's code\n" +
			"(128 plus the signal's number when a signal ended it). Pass the owner's pipe as an inherited file descriptor\n" +
			"with --owner-fd; the owner never writes to it, and every way the owner can die closes it. Where the owner\n" +
			"cannot pass a pipe, as Node on Windows cannot, pass its process id with --owner-pid instead. On Windows the\n" +
			"program runs in a Job Object that ends with the reaper, however the reaper ends.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			logger, closeLog, err := openLog(logFile)
			if err != nil {
				*code = ExitInternal
				return err
			}
			defer closeLog()
			signals := make(chan os.Signal, 1)
			signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
			defer signal.Stop(signals)
			cfg := reaper.Config{
				Path:     args[0],
				Args:     args[1:],
				Grace:    grace,
				Registry: registry,
				Signals:  signals,
				Logger:   logger,
			}
			if ownerFD >= 0 {
				cfg.Owner = reaper.OwnerPipe(ownerFD)
			}
			cfg.OwnerPID = ownerPID
			if slotDir != "" {
				level, err := parseLevel(holdAt)
				if err != nil {
					*code = ExitUsage
					return err
				}
				cfg.Slots = &slots.Config{Dir: slotDir, Count: slotCount, Pressure: pressure.Read, HoldAt: level}
				cfg.SlotWait = slotWait
			}
			exit, err := cfg.Run(cmd.Context())
			*code = exit
			return err
		},
	}
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().IntVar(&ownerFD, "owner-fd", -1, "inherited file descriptor of the owner's pipe; the program is stopped when it closes")
	cmd.Flags().IntVar(&ownerPID, "owner-pid", 0, "the owner's process id; the program is stopped when it exits (use on Windows, where Node cannot pass --owner-fd)")
	cmd.Flags().DurationVar(&grace, "grace", proc.DefaultGrace, "time between SIGTERM and the hard stop")
	cmd.Flags().StringVar(&registry, "registry", "", "folder to record the program in while it runs, for a later sweep")
	cmd.Flags().StringVar(&logFile, "log-file", "", "file to append what the reaper does to; by default nothing is logged")
	cmd.Flags().StringVar(&slotDir, "slots", "", "folder of slots shared with other programs; a slot is taken before the program starts")
	cmd.Flags().IntVar(&slotCount, "slot-count", 8, "how many slots the --slots folder has; every program sharing it must agree")
	cmd.Flags().DurationVar(&slotWait, "slot-wait", 30*time.Second, "how long to wait for a slot before exiting 75 without starting the program")
	cmd.Flags().StringVar(&holdAt, "hold-at", "critical", "memory pressure at which slots are held back: warning or critical")
	return cmd
}

func newPressure(stdout io.Writer, code *int) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "pressure",
		Short: "Show how short of memory the machine is",
		Long: "Reads the platform's own memory pressure signal: kern.memorystatus_vm_pressure_level on macOS, pressure\n" +
			"stall information (or available memory) on Linux, and the low-memory notification and memory load on Windows.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := pressure.Read(cmd.Context())
			if err != nil {
				*code = ExitInternal
				return err
			}
			if asJSON {
				return encodeJSON(stdout, r, code)
			}
			writeln(stdout, "Memory pressure")
			writeln(stdout, "  Level:   "+r.Level.String()+"  ("+levelGloss[r.Level]+")")
			writeln(stdout, "  Source:  "+r.Source)
			writeln(stdout, "  Detail:  "+r.Detail)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the reading as JSON")
	return cmd
}

func newSlots(stdout io.Writer, code *int) *cobra.Command {
	var dir string
	var count int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "slots --dir <folder>",
		Short: "Show how many slots in a shared folder are free",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			free, err := slots.Config{Dir: dir, Count: count}.Free(cmd.Context())
			if err != nil {
				*code = ExitInternal
				return err
			}
			if asJSON {
				return encodeJSON(stdout, map[string]any{"dir": dir, "count": count, "free": free}, code)
			}
			writeln(stdout, "Slots")
			writeln(stdout, "  Folder:  "+dir)
			writeln(stdout, fmt.Sprintf("  Count:   %d", count))
			writeln(stdout, fmt.Sprintf("  Free:    %d", free))
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "the shared slots folder")
	cmd.Flags().IntVar(&count, "count", 8, "how many slots the folder has")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the counts as JSON")
	if err := cmd.MarkFlagRequired("dir"); err != nil {
		panic(err)
	}
	return cmd
}

var levelGloss = map[pressure.Level]string{
	pressure.Normal:   "memory is not short",
	pressure.Warning:  "memory is getting short; a good time to stop starting things",
	pressure.Critical: "the system is reclaiming memory hard; close what can be closed",
}

func parseLevel(name string) (pressure.Level, error) {
	switch name {
	case "warning":
		return pressure.Warning, nil
	case "critical":
		return pressure.Critical, nil
	default:
		return pressure.Normal, fmt.Errorf("--hold-at must be warning or critical, not %q", name)
	}
}

func encodeJSON(w io.Writer, v any, code *int) error {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		*code = ExitInternal
		return err
	}
	return nil
}

// startTracing sends spans to an OpenTelemetry collector when the standard OTEL_EXPORTER_OTLP_ENDPOINT (or its
// traces form) is set, and does nothing otherwise. It returns the function that flushes and stops tracing.
func startTracing(stderr io.Writer) func() {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return func() {}
	}
	shutdown, err := olly.Init(olly.Config{Enabled: true, ServiceName: "processconcierge", AllowOTLPFailure: true})
	if err != nil {
		writeln(stderr, "processconcierge: tracing is off: "+err.Error())
		return func() {}
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	}
}

func newSweep(stdout io.Writer, code *int) *cobra.Command {
	var registry string
	var dryRun, asJSON bool
	cmd := &cobra.Command{
		Use:   "sweep --registry <folder>",
		Short: "Stop the programs a registry still lists as running, such as after a crash",
		Long: "Reads each entry in the registry folder. An entry whose program still runs, with the start time the entry\n" +
			"records, has its process group killed; an entry whose pid is gone or now belongs to another program is left\n" +
			"alone. Every entry is then removed. --dry-run reports the same and changes nothing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			results, err := sweep.Registry{Dir: registry}.Sweep(cmd.Context(), dryRun)
			if err != nil {
				*code = ExitInternal
				return err
			}
			if asJSON {
				return encodeJSON(stdout, results, code)
			}
			printSweep(stdout, registry, dryRun, results)
			return nil
		},
	}
	cmd.Flags().StringVar(&registry, "registry", "", "the registry folder that run --registry wrote")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be stopped, and change nothing")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the results as JSON")
	if err := cmd.MarkFlagRequired("registry"); err != nil {
		panic(err)
	}
	return cmd
}

func openLog(file string) (*slog.Logger, func(), error) {
	if file == "" {
		return slog.New(slog.NewTextHandler(io.Discard, nil)), func() {}, nil
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G304: the operator names the log file.
	if err != nil {
		return nil, nil, fmt.Errorf("open the log file: %w", err)
	}
	return slog.New(slog.NewTextHandler(f, nil)), func() { _ = f.Close() }, nil
}

func printGuide(w io.Writer, version string) {
	writeln(w, "processconcierge "+version)
	writeln(w, "Supervises child processes so nothing they start outlives them, on macOS, Linux and Windows.")
	writeln(w, "")
	writeln(w, "Documentation for agents")
	writeln(w, "  AGENTS.md")
	writeln(w, "  README.md")
	writeln(w, "")
	writeln(w, "Read-only")
	writeln(w, "  version, help, pressure, slots, sweep --dry-run")
	writeln(w, "")
	writeln(w, "Runs and stops processes")
	writeln(w, "  run      run a program in its own unit, stopped with everything it started")
	writeln(w, "  sweep    stop what a registry still lists as running")
	writeln(w, "")
	writeln(w, "Automation")
	writeln(w, "  Nothing prompts. Run sweep --dry-run first to see what it would stop.")
	writeln(w, "  run exits with its program's code, 127 when the program could not start, and 75 when no slot came free.")
	writeln(w, "  Set OTEL_EXPORTER_OTLP_ENDPOINT to send spans for each start, stop, sweep and slot.")
	writeln(w, "")
	writeln(w, "Full command list: processconcierge help")
}

func printSweep(w io.Writer, registry string, dryRun bool, results []sweep.Result) {
	writeln(w, "Sweep")
	writeln(w, "  Registry:  "+registry)
	writeln(w, "  Dry run:   "+map[bool]string{true: "yes", false: "no"}[dryRun])
	writeln(w, "")
	writeln(w, "Entries")
	if len(results) == 0 {
		writeln(w, "  none")
		return
	}
	for _, r := range results {
		writeln(w, fmt.Sprintf("  - pid %d", r.Entry.PID))
		writeln(w, "      Started:  "+time.UnixMilli(r.Entry.StartedMS).Format(time.RFC3339))
		writeln(w, "      Outcome:  "+string(r.Outcome)+"  ("+outcomeGloss[r.Outcome]+")")
	}
}

var outcomeGloss = map[sweep.Outcome]string{
	sweep.Stopped:   "it was still running, and its group was killed",
	sweep.WouldStop: "it is still running, and a sweep would kill its group",
	sweep.Gone:      "no process has that pid any more",
	sweep.Reused:    "another program now has that pid, so it was left alone",
}

func writeln(w io.Writer, line string) {
	_, _ = fmt.Fprintln(w, line)
}
