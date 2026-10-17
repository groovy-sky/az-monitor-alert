package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/groovy-sky/az-monitor-alert/execution"
)

func TestExampleProcess(t *testing.T) {
	if os.Getenv("ALERT_EXAMPLE_TEST_PROCESS") == "1" {
		os.Exit(run())
	}
}

func exampleCommand(t *testing.T, mode, timeout string) *exec.Cmd {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestExampleProcess$")
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "ALERT_EXAMPLE_TEST_PROCESS", "EXAMPLE_MODE", "EXECUTION_TIMEOUT", "EXAMPLE_DELAY", "WORKLOAD", "EXECUTION_ID":
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "ALERT_EXAMPLE_TEST_PROCESS=1", "EXAMPLE_MODE="+mode,
		"EXECUTION_TIMEOUT="+timeout, "EXAMPLE_DELAY=0s", "WORKLOAD=cli-test", "EXECUTION_ID=logical-cli")
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func TestExampleExitAndEvents(t *testing.T) {
	for _, tc := range []struct {
		mode, timeout, event, code string
		exit                       int
	}{
		{"success", "10m", "succeeded", "", 0},
		{"failure", "10m", "failed", "work_failed", 1},
		{"panic", "10m", "failed", "panic", 1},
		{"wait", "1ms", "failed", "timeout", 1},
		{"invalid", "10m", "", "", 2},
		{"success", "invalid", "", "", 2},
	} {
		t.Run(tc.mode+"/"+tc.timeout, func(t *testing.T) {
			cmd := exampleCommand(t, tc.mode, tc.timeout)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			output, err := cmd.Output()
			exit := 0
			if err != nil {
				var exitError *exec.ExitError
				if !errors.As(err, &exitError) {
					t.Fatal(err)
				}
				exit = exitError.ExitCode()
			}
			if exit != tc.exit {
				t.Fatalf("exit %d, want %d; stderr: %s", exit, tc.exit, stderr.String())
			}
			if tc.event == "" {
				if len(output) != 0 {
					t.Fatal("configuration error emitted execution events")
				}
				return
			}
			lines := bytes.Split(bytes.TrimSpace(output), []byte("\n"))
			if len(lines) != 2 {
				t.Fatalf("expected two events: %s", output)
			}
			var terminal execution.Event
			if err := json.Unmarshal(lines[1], &terminal); err != nil {
				t.Fatal(err)
			}
			if terminal.Event != tc.event || terminal.ErrorCode != tc.code ||
				terminal.ExecutionID != "logical-cli" || terminal.Workload != "cli-test" {
				t.Fatalf("unexpected event: %+v", terminal)
			}
			if strings.Contains(stderr.String(), "goroutine") || strings.Contains(stderr.String(), "example panic") {
				t.Fatal("raw panic or stack leaked")
			}
		})
	}
}

func TestGracefulSignals(t *testing.T) {
	for _, signal := range []os.Signal{syscall.SIGTERM, syscall.SIGINT} {
		t.Run(signal.String(), func(t *testing.T) {
			cmd := exampleCommand(t, "wait", "10m")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			// Bound the test even if graceful shutdown regresses.
			timer := time.AfterFunc(5*time.Second, func() { _ = cmd.Process.Kill() })
			defer timer.Stop()
			scanner := bufio.NewScanner(stdout)
			if !scanner.Scan() {
				t.Fatal("missing start")
			}
			if err := cmd.Process.Signal(signal); err != nil {
				t.Fatal(err)
			}
			if !scanner.Scan() {
				t.Fatal("missing cancellation terminal")
			}
			var terminal execution.Event
			if err := json.Unmarshal(scanner.Bytes(), &terminal); err != nil {
				t.Fatal(err)
			}
			if terminal.Event != "failed" || terminal.ErrorCode != "canceled" {
				t.Fatalf("unexpected signal outcome: %+v", terminal)
			}
			if scanner.Scan() || scanner.Err() != nil {
				t.Fatal("unexpected extra event or scan error")
			}
			var exitError *exec.ExitError
			if err := cmd.Wait(); !errors.As(err, &exitError) || exitError.ExitCode() != 1 {
				t.Fatalf("signal exit must be nonzero: %v", err)
			}
		})
	}
}
