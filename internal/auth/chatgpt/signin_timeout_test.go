package chatgpt

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSignInContextInteractionWindow(t *testing.T) {
	before := time.Now()
	ctx, cancel := signInContext(context.Background())
	defer cancel()
	after := time.Now()
	deadline, ok := ctx.Deadline()
	if !ok || deadline.Before(before.Add(30*time.Minute)) || deadline.After(after.Add(30*time.Minute)) {
		t.Fatal("sign-in interaction deadline is not 30 minutes")
	}
	cancel()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("sign-in context ignored cancellation")
	}
}

func TestSignInContextPreservesCallerDeadlineAndCancellation(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	parent, cancelParent := context.WithDeadline(context.Background(), deadline)
	defer cancelParent()
	ctx, cancel := signInContext(parent)
	defer cancel()
	actual, ok := ctx.Deadline()
	if !ok || !actual.Equal(deadline) {
		t.Fatal("sign-in extended an earlier caller deadline")
	}
	cancelParent()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("sign-in ignored parent cancellation")
	}
}
