// Package execution reports handled work attempts as JSON console events.
package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
)

// Errors are deliberately fixed summaries: underlying work, panic, randomness,
// and writer errors are never included in events or returned errors.
var (
	ErrWorkFailed = errors.New("work failed")
	ErrPanic      = errors.New("work panicked")
	ErrCanceled   = errors.New("work canceled")
	ErrTimeout    = errors.New("work deadline exceeded")
	ErrOutput     = errors.New("event output failed")
	ErrRandom     = errors.New("execution ID generation failed")
	ErrInput      = errors.New("invalid execution input")
)

// Event is the versioned console-log contract. DurationMS is absent on starts.
type Event struct {
	Schema        string `json:"schema"`
	SchemaVersion int    `json:"schema_version"`
	Event         string `json:"event"`
	Timestamp     string `json:"timestamp"`
	Workload      string `json:"workload"`
	ExecutionID   string `json:"execution_id"`
	AttemptID     string `json:"attempt_id"`
	DurationMS    *int64 `json:"duration_ms,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
	ErrorSummary  string `json:"error_summary,omitempty"`
}

// Logger serializes writes for concurrent Run calls sharing this instance.
// Use an unbuffered writer such as os.Stdout, or flush your writer after Run.
type Logger struct {
	mu     sync.Mutex
	writer io.Writer
}

func NewLogger(writer io.Writer) *Logger {
	return &Logger{writer: writer}
}

// Run emits a start and one terminal outcome, unless output fails. It generates
// an execution ID when empty and always generates a new attempt ID. Identifiers
// must be 1-128 ASCII letters, digits, dots, underscores, colons, or hyphens.
// Work must honor ctx; a context cannot forcibly stop arbitrary Go code.
// A panic in work on this goroutine becomes ErrPanic, never a successful result.
// Run does not recover panics in goroutines started by work.
func (l *Logger) Run(ctx context.Context, workload, executionID string, work func(context.Context) error) (result error) {
	if ctx == nil || work == nil || !validID(workload) || (executionID != "" && !validID(executionID)) {
		return ErrInput
	}
	if executionID == "" {
		var err error
		executionID, err = randomID()
		if err != nil {
			return err
		}
	}
	attemptID, err := randomID()
	if err != nil {
		return err
	}
	start := time.Now()
	event := Event{
		Schema: "az-monitor-alert.execution", SchemaVersion: 1,
		Event: "started", Timestamp: start.UTC().Format(time.RFC3339Nano),
		Workload: workload, ExecutionID: executionID, AttemptID: attemptID,
	}
	if err := l.write(event); err != nil {
		return err
	}
	completed := false
	defer func() {
		// completed also detects panic(nil), including legacy panicnil behavior.
		if !completed {
			_ = recover()
			result = ErrPanic
		}
		event.Event = "succeeded"
		elapsed := max(int64(0), time.Since(start).Milliseconds())
		event.DurationMS = &elapsed
		event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
		if result != nil {
			event.Event = "failed"
			switch result {
			case ErrPanic:
				event.ErrorCode, event.ErrorSummary = "panic", "Work panicked"
			case ErrCanceled:
				event.ErrorCode, event.ErrorSummary = "canceled", "Work canceled"
			case ErrTimeout:
				event.ErrorCode, event.ErrorSummary = "timeout", "Work deadline exceeded"
			default:
				event.ErrorCode, event.ErrorSummary = "work_failed", "Work failed"
			}
		}
		if err := l.write(event); err != nil {
			result = errors.Join(result, err)
		}
	}()
	if ctx.Err() == nil {
		if err := work(ctx); err != nil {
			result = ErrWorkFailed
		}
	}
	// Cancellation observed before returning wins over ordinary success/error.
	switch ctx.Err() {
	case context.Canceled:
		result = ErrCanceled
	case context.DeadlineExceeded:
		result = ErrTimeout
	}
	completed = true
	return result
}

func (l *Logger) write(event Event) error {
	data, err := json.Marshal(event)
	if err != nil {
		return ErrOutput
	}
	data = append(data, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.writer == nil {
		return ErrOutput
	}
	n, err := l.writer.Write(data)
	if err != nil || n != len(data) {
		return ErrOutput
	}
	return nil
}

func randomID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", ErrRandom
	}
	return hex.EncodeToString(id[:]), nil
}

func validID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-') {
			return false
		}
	}
	return true
}
