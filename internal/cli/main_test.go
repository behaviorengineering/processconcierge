package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/behaviorengineering/processconcierge/internal/proctest"
)

// binary is the processconcierge command, built once for these tests, so each test drives it as its owner would.
var binary string

func TestMain(m *testing.M) {
	proctest.RunHelperIfAsked()
	dir, err := os.MkdirTemp("", "processconcierge-test-")
	if err != nil {
		panic(err)
	}
	if binary, err = proctest.BuildCommand(dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestTheGuideVersionAndAnUnknownCommand(t *testing.T) {
	var out bytes.Buffer
	if code := Execute(context.Background(), nil, &out, &out, "v1.2.3"); code != ExitOK {
		t.Fatalf("bare invoke exited %d", code)
	}
	if lines := strings.Count(out.String(), "\n"); lines < 10 || !strings.HasPrefix(out.String(), "processconcierge v1.2.3\n") {
		t.Fatalf("guide:\n%s", out.String())
	}
	out.Reset()
	if code := Execute(context.Background(), []string{"version"}, &out, &out, "v1.2.3"); code != ExitOK || out.String() != "processconcierge v1.2.3\n" {
		t.Fatalf("version exited %d: %q", code, out.String())
	}
	out.Reset()
	if code := Execute(context.Background(), []string{"nope"}, &out, &out, "v1.2.3"); code != ExitUsage {
		t.Fatalf("an unknown command exited %d", code)
	}
}
