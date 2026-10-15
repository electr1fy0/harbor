package main

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestSIGTERMInterruptsStartupRestoreAndRecovery(t *testing.T) {
	for _, phase := range []string{"restore", "recover"} {
		t.Run(phase, func(t *testing.T) {
			ctx, stop := startupSignalContext()
			defer stop()

			started := make(chan struct{})
			recoveryStarted := make(chan struct{}, 1)
			result := make(chan [2]error, 1)
			go func() {
				restoreErr, recoveryErr := restoreAndRecover(ctx,
					func(restoreCtx context.Context) error {
						if phase != "restore" {
							return nil
						}
						close(started)
						<-restoreCtx.Done()
						return restoreCtx.Err()
					},
					func(recoveryCtx context.Context) error {
						recoveryStarted <- struct{}{}
						if phase != "recover" {
							return nil
						}
						close(started)
						<-recoveryCtx.Done()
						return recoveryCtx.Err()
					},
				)
				result <- [2]error{restoreErr, recoveryErr}
			}()

			if phase == "recover" {
				select {
				case <-recoveryStarted:
				case <-time.After(time.Second):
					t.Fatal("recovery did not start after a successful restore")
				}
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("startup phase did not begin")
			}

			process, err := os.FindProcess(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			if err := process.Signal(syscall.SIGTERM); err != nil {
				t.Fatalf("send SIGTERM to test process: %v", err)
			}

			select {
			case got := <-result:
				if phase == "restore" {
					if !errors.Is(got[0], context.Canceled) || !errors.Is(got[1], context.Canceled) {
						t.Fatalf("startup results after SIGTERM = (%v, %v), want canceled restore and skipped recovery", got[0], got[1])
					}
				} else if !errors.Is(got[1], context.Canceled) {
					t.Fatalf("recovery error after SIGTERM = %v, want context.Canceled", got[1])
				}
			case <-time.After(time.Second):
				t.Fatal("startup did not exit promptly after SIGTERM")
			}
		})
	}
}
