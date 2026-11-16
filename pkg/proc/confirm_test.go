package proc

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConfirmGoneReturnsOnceTheChildIsGone(t *testing.T) {
	done := make(chan struct{})
	close(done)
	if err := confirmGone(context.Background(), done, time.Second, func() bool { return false }); err != nil {
		t.Fatal(err)
	}
}

func TestConfirmGoneCallsAChildThatOutlivesTheWaitAndIsNoZombieStuck(t *testing.T) {
	err := confirmGone(context.Background(), make(chan struct{}), 10*time.Millisecond, func() bool { return false })
	if !errors.Is(err, errStuck) {
		t.Fatalf("err = %v, want errStuck", err)
	}
}

func TestConfirmGoneWaitsOnAZombieUntilItIsCollected(t *testing.T) {
	done := make(chan struct{})
	go func() { time.Sleep(50 * time.Millisecond); close(done) }()
	if err := confirmGone(context.Background(), done, 10*time.Millisecond, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
}

func TestConfirmGoneStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := confirmGone(ctx, make(chan struct{}), time.Second, func() bool { return false }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestAStartThatFailedAfterTheChildExistedIsNeverRetried(t *testing.T) {
	if retryable(nil) {
		t.Fatal("no error is retryable")
	}
	if retryable(errors.Join(errAfterCreate, errors.New("could not join the job"))) {
		t.Fatal("a start that failed after the child existed is retryable")
	}
}
