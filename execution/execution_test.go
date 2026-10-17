package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func decodeEvents(t *testing.T, output string) []Event {
	t.Helper()
	var events []Event
	decoder := json.NewDecoder(strings.NewReader(output))
	for {
		var event Event
		err := decoder.Decode(&event)
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
}

func TestOutcomes(t *testing.T) {
	const sensitive = "******"
	for _, tc := range []struct {
		name string
		work func(context.Context) error
		ctx  func() (context.Context, context.CancelFunc)
		want error
		code string
	}{
		{name: "success", work: func(context.Context) error { return nil }},
		{name: "error", work: func(context.Context) error { return errors.New(sensitive) }, want: ErrWorkFailed, code: "work_failed"},
		{name: "panic", work: func(context.Context) error { panic(sensitive) }, want: ErrPanic, code: "panic"},
		{name: "nil panic", work: func(context.Context) error { panic(nil) }, want: ErrPanic, code: "panic"},
		{name: "canceled", ctx: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}, work: func(context.Context) error { t.Fatal("pre-canceled work must not run"); return nil }, want: ErrCanceled, code: "canceled"},
		{name: "deadline", ctx: func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}, work: func(context.Context) error { t.Fatal("expired work must not run"); return nil }, want: ErrTimeout, code: "timeout"},
		{name: "cooperative cancellation", work: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}, ctx: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(time.Millisecond, cancel)
			return ctx, cancel
		}, want: ErrCanceled, code: "canceled"},
		{name: "cooperative timeout", work: func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		}, ctx: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), time.Millisecond)
		}, want: ErrTimeout, code: "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.ctx != nil {
				var cancel context.CancelFunc
				ctx, cancel = tc.ctx()
				defer cancel()
			}
			var output bytes.Buffer
			before := time.Now()
			err := NewLogger(&output).Run(ctx, "test-work", "logical-123", tc.work)
			after := time.Now()
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if strings.Contains(output.String(), sensitive) || err != nil && strings.Contains(err.Error(), sensitive) {
				t.Fatal("sensitive value leaked")
			}
			events := decodeEvents(t, output.String())
			if len(events) != 2 {
				t.Fatalf("got %d events, want 2", len(events))
			}
			for _, event := range events {
				if event.Schema != "az-monitor-alert.execution" || event.SchemaVersion != 1 ||
					event.Workload != "test-work" || event.ExecutionID != "logical-123" || len(event.AttemptID) != 32 {
					t.Fatalf("invalid event: %+v", event)
				}
				ts, err := time.Parse(time.RFC3339Nano, event.Timestamp)
				if err != nil || !strings.HasSuffix(event.Timestamp, "Z") || ts.Before(before) || ts.After(after) {
					t.Fatalf("invalid UTC time %q: %v", event.Timestamp, err)
				}
			}
			if events[0].Event != "started" || events[0].DurationMS != nil ||
				events[0].AttemptID != events[1].AttemptID {
				t.Fatal("invalid start or correlation")
			}
			terminal := events[1]
			wantEvent := "succeeded"
			if tc.want != nil {
				wantEvent = "failed"
			}
			if terminal.Event != wantEvent || terminal.DurationMS == nil || *terminal.DurationMS < 0 ||
				terminal.ErrorCode != tc.code || (terminal.ErrorSummary != "") != (tc.want != nil) {
				t.Fatalf("invalid terminal event: %+v", terminal)
			}
		})
	}
}

func TestIDsAndConcurrentWrites(t *testing.T) {
	var output bytes.Buffer
	logger := NewLogger(&output)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := logger.Run(context.Background(), "worker", "", func(context.Context) error { return nil }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	events := decodeEvents(t, output.String())
	if len(events) != 60 {
		t.Fatalf("got %d events", len(events))
	}
	starts := make(map[string]Event)
	executions := make(map[string]bool)
	for _, event := range events {
		if event.Event == "started" {
			if _, exists := starts[event.AttemptID]; exists || executions[event.ExecutionID] {
				t.Fatal("duplicate generated ID")
			}
			starts[event.AttemptID] = event
			executions[event.ExecutionID] = true
		} else if start, exists := starts[event.AttemptID]; !exists || start.ExecutionID != event.ExecutionID {
			t.Fatal("terminal without matching start")
		}
	}
	output.Reset()
	for i := 0; i < 2; i++ {
		if err := logger.Run(context.Background(), "worker", "retry", func(context.Context) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	events = decodeEvents(t, output.String())
	if events[0].ExecutionID != events[2].ExecutionID || events[0].AttemptID == events[2].AttemptID {
		t.Fatal("retries must preserve logical ID and change attempt ID")
	}
}

type failingWriter struct {
	calls     int
	failAfter int
	short     bool
}

func (w *failingWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls > w.failAfter {
		if w.short {
			return len(p) - 1, nil
		}
		return 0, errors.New("sensitive writer failure")
	}
	return len(p), nil
}

func TestWriterFailure(t *testing.T) {
	for _, failAfter := range []int{0, 1} {
		for _, short := range []bool{false, true} {
			writer := &failingWriter{failAfter: failAfter, short: short}
			ran := false
			err := NewLogger(writer).Run(context.Background(), "worker", "", func(context.Context) error {
				ran = true
				return errors.New("sensitive work failure")
			})
			if !errors.Is(err, ErrOutput) || strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("unsafe or missing output error: %v", err)
			}
			if ran != (failAfter == 1) || writer.calls != failAfter+1 {
				t.Fatal("work ran without a start, or output was retried")
			}
			if ran && !errors.Is(err, ErrWorkFailed) {
				t.Fatal("terminal output failure lost work failure")
			}
		}
	}
	if err := NewLogger(nil).Run(context.Background(), "worker", "", func(context.Context) error { return nil }); err != ErrOutput {
		t.Fatal(err)
	}
}

func TestInvalidInput(t *testing.T) {
	for _, id := range []string{"contains space", "line\nbreak", "non-ascii-ä", strings.Repeat("a", 129)} {
		var output bytes.Buffer
		if err := NewLogger(&output).Run(context.Background(), "worker", id, func(context.Context) error { return nil }); err != ErrInput || output.Len() != 0 {
			t.Fatalf("accepted invalid ID %q", id)
		}
	}
}
