// Package pressure reads how short of memory the machine is, from each platform's own signal: the kernel's memory
// pressure level on macOS, pressure stall information on Linux (with available memory as a fallback), and the
// low-memory notification and memory load on Windows. A supervisor reads it to hold new starts while the machine is
// short, and tells its app, which decides what to close.
package pressure

import (
	"bufio"
	"context"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/behaviorengineering/processconcierge/pkg/procerr"
)

// Level is how short of memory the machine is.
type Level int

const (
	// Normal means memory is not short.
	Normal Level = iota
	// Warning means memory is getting short: a good time to stop starting things.
	Warning
	// Critical means memory is short enough that the system is reclaiming it hard: close what can be closed.
	Critical
)

// String names the level.
func (l Level) String() string {
	switch l {
	case Normal:
		return "normal"
	case Warning:
		return "warning"
	case Critical:
		return "critical"
	default:
		return "level(" + strconv.Itoa(int(l)) + ")"
	}
}

// MarshalText writes the level's name, so JSON shows "warning", not 1.
func (l Level) MarshalText() ([]byte, error) {
	return []byte(l.String()), nil
}

// Reading is one look at the machine's memory.
type Reading struct {
	// Level is how short of memory the machine is.
	Level Level `json:"level"`
	// Source names the signal the level came from, such as "kern.memorystatus_vm_pressure_level".
	Source string `json:"source"`
	// Detail is the raw value read, for people.
	Detail string `json:"detail"`
}

// Thresholds a Linux or Windows reading is judged by. On macOS the system reports its level itself.
const (
	// PSIWarning is the share of the last ten seconds, in percent, that some task stalled on memory.
	PSIWarning = 10.0
	// PSICritical is the share of the last ten seconds, in percent, that every task stalled on memory.
	PSICritical = 10.0
	// AvailableWarning and AvailableCritical are the percent of memory still available.
	AvailableWarning  = 10.0
	AvailableCritical = 5.0
)

var tracer = otel.Tracer("github.com/behaviorengineering/processconcierge/pkg/pressure")

// Read looks at the machine's memory once.
func Read(ctx context.Context) (Reading, error) {
	const op = "pressure.Read"
	if ctx == nil {
		return Reading{}, procerr.New(procerr.CodeInvalid, op, "context is required")
	}
	r, err := read(ctx)
	if err != nil {
		return Reading{}, procerr.Wrap(err, procerr.CodePressure, op, "read memory pressure")
	}
	return r, nil
}

// Watch reads every interval until ctx ends, and sends the first reading and each one whose level differs from the
// last. Each change is also a span, so a trace shows when the machine got short. A reading that fails is skipped.
// The channel closes when ctx ends.
func Watch(ctx context.Context, interval time.Duration) <-chan Reading {
	out := make(chan Reading, 1)
	if ctx == nil {
		close(out)
		return out
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	go func() {
		defer close(out)
		last := Level(-1)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if r, err := read(ctx); err == nil && r.Level != last {
				_, span := tracer.Start(ctx, "processconcierge.pressure.change")
				span.SetAttributes(attribute.String("pressure.level", r.Level.String()),
					attribute.String("pressure.source", r.Source), attribute.String("pressure.detail", r.Detail))
				span.End()
				last = r.Level
				select {
				case out <- r:
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return out
}

// levelFromPSI judges Linux's /proc/pressure/memory: "full" over PSICritical is critical, "some" over PSIWarning is a
// warning. It reports false when the text holds neither line.
func levelFromPSI(text string) (Level, string, bool) {
	some, okSome := psiAvg10(text, "some")
	full, okFull := psiAvg10(text, "full")
	if !okSome && !okFull {
		return Normal, "", false
	}
	detail := "some avg10=" + strconv.FormatFloat(some, 'f', 2, 64) + " full avg10=" + strconv.FormatFloat(full, 'f', 2, 64)
	switch {
	case full >= PSICritical:
		return Critical, detail, true
	case some >= PSIWarning:
		return Warning, detail, true
	default:
		return Normal, detail, true
	}
}

func psiAvg10(text, kind string) (float64, bool) {
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] != kind {
			continue
		}
		for _, field := range fields[1:] {
			if value, ok := strings.CutPrefix(field, "avg10="); ok {
				f, err := strconv.ParseFloat(value, 64)
				return f, err == nil
			}
		}
	}
	return 0, false
}

// levelFromAvailable judges the percent of memory still available.
func levelFromAvailable(availablePercent float64) Level {
	switch {
	case availablePercent < AvailableCritical:
		return Critical
	case availablePercent < AvailableWarning:
		return Warning
	default:
		return Normal
	}
}

// levelFromDarwin turns macOS's kern.memorystatus_vm_pressure_level (1 normal, 2 warning, 4 critical) into a level.
func levelFromDarwin(value uint32) Level {
	switch {
	case value >= 4:
		return Critical
	case value >= 2:
		return Warning
	default:
		return Normal
	}
}
