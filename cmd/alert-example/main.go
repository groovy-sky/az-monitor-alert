package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/groovy-sky/az-monitor-alert/execution"
)

func main() {
	// run returns only after deferred terminal logging and signal cleanup.
	os.Exit(run())
}

func run() int {
	mode := env("EXAMPLE_MODE", "success")
	timeout, err := time.ParseDuration(env("EXECUTION_TIMEOUT", "10m"))
	delay, delayErr := time.ParseDuration(env("EXAMPLE_DELAY", "0s"))
	if err != nil || delayErr != nil || timeout <= 0 || delay < 0 ||
		(mode != "success" && mode != "failure" && mode != "panic" && mode != "wait") {
		fmt.Fprintln(os.Stderr, "invalid example configuration")
		return 2
	}
	signals, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(signals, timeout)
	defer cancel()
	err = execution.NewLogger(os.Stdout).Run(ctx, env("WORKLOAD", "example"), os.Getenv("EXECUTION_ID"), func(ctx context.Context) error {
		// Placeholder only: replace this function with actual cooperative work.
		if mode == "wait" {
			<-ctx.Done()
			return ctx.Err()
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		switch mode {
		case "failure":
			return errors.New("example failure")
		case "panic":
			panic("example panic")
		default:
			return nil
		}
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
