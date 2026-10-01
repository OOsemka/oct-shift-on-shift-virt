package logging

import (
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
)

type Logger struct {
	inner         *slog.Logger
	cluster       string
	phase         string
	node          string
	component     string
	correlationID string
}

func New() *Logger {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	return &Logger{
		inner:     slog.New(handler),
		component: "orchestrator",
	}
}

func (l *Logger) WithCluster(cluster string) *Logger {
	return &Logger{
		inner:         l.inner,
		cluster:       cluster,
		phase:         l.phase,
		node:          l.node,
		component:     l.component,
		correlationID: l.correlationID,
	}
}

func (l *Logger) WithPhase(phase string) *Logger {
	return &Logger{
		inner:         l.inner,
		cluster:       l.cluster,
		phase:         phase,
		node:          l.node,
		component:     l.component,
		correlationID: l.correlationID,
	}
}

func (l *Logger) WithNode(node string) *Logger {
	return &Logger{
		inner:         l.inner,
		cluster:       l.cluster,
		phase:         l.phase,
		node:          node,
		component:     l.component,
		correlationID: l.correlationID,
	}
}

func (l *Logger) WithComponent(component string) *Logger {
	return &Logger{
		inner:         l.inner,
		cluster:       l.cluster,
		phase:         l.phase,
		node:          l.node,
		component:     component,
		correlationID: l.correlationID,
	}
}

func (l *Logger) WithCorrelation() *Logger {
	return &Logger{
		inner:         l.inner,
		cluster:       l.cluster,
		phase:         l.phase,
		node:          l.node,
		component:     l.component,
		correlationID: uuid.New().String(),
	}
}

func (l *Logger) attrs() []any {
	attrs := []any{}
	if l.cluster != "" {
		attrs = append(attrs, "cluster", l.cluster)
	}
	if l.phase != "" {
		attrs = append(attrs, "phase", l.phase)
	}
	if l.node != "" {
		attrs = append(attrs, "node", l.node)
	}
	if l.component != "" {
		attrs = append(attrs, "component", l.component)
	}
	if l.correlationID != "" {
		attrs = append(attrs, "correlation_id", l.correlationID)
	}
	return attrs
}

func (l *Logger) Info(msg string, args ...any) {
	all := append(l.attrs(), args...)
	l.inner.Info(msg, all...)
}

func (l *Logger) Error(msg string, args ...any) {
	all := append(l.attrs(), args...)
	l.inner.Error(msg, all...)
}

func (l *Logger) Warn(msg string, args ...any) {
	all := append(l.attrs(), args...)
	l.inner.Warn(msg, all...)
}

func (l *Logger) Debug(msg string, args ...any) {
	all := append(l.attrs(), args...)
	l.inner.Debug(msg, all...)
}

type TimedOperation struct {
	logger *Logger
	op     string
	start  time.Time
}

func (l *Logger) StartOperation(operation string) *TimedOperation {
	l.Info("operation started", "operation", operation)
	return &TimedOperation{
		logger: l,
		op:     operation,
		start:  time.Now(),
	}
}

func (t *TimedOperation) Done(err error) {
	durationMs := time.Since(t.start).Milliseconds()
	if err != nil {
		t.logger.Error("operation failed", "operation", t.op, "duration_ms", durationMs, "error", err.Error())
	} else {
		t.logger.Info("operation completed", "operation", t.op, "duration_ms", durationMs)
	}
}
