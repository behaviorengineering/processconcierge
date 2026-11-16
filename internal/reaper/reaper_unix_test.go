//go:build unix

package reaper

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/behaviorengineering/processconcierge/internal/proctest"
)

func TestASecondStopRequestSkipsTheGracePeriod(t *testing.T) {
	file := filepath.Join(t.TempDir(), "grandchild")
	signals := make(chan os.Signal, 2)
	cfg := Config{
		Path: "/bin/sh",
		// Both ignore SIGTERM, so only the hard stop ends them before the ten-second grace period.
		Args:    []string{"-c", "trap '' TERM; sleep 60 & echo $! > " + file + "; wait"},
		Stdin:   strings.NewReader(""),
		Stdout:  io.Discard,
		Stderr:  io.Discard,
		Grace:   10 * time.Second,
		Signals: signals,
	}
	go func() {
		grandchild := proctest.ReadPid(t, file)
		signals <- syscall.SIGTERM
		time.Sleep(300 * time.Millisecond)
		signals <- syscall.SIGTERM
		proctest.WaitGone(t, grandchild)
	}()
	began := time.Now()
	code, err := cfg.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("Run took %v, as if the second request waited out the grace period", took)
	}
	if code != 128+int(syscall.SIGKILL) {
		t.Fatalf("code = %d, want %d for a child the hard stop ended", code, 128+int(syscall.SIGKILL))
	}
}
