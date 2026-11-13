package usage

import (
	"context"
	"errors"
	"testing"
)

type drainingTestSink struct {
	entered chan struct{}
	release chan struct{}
	count   int
}

func (s *drainingTestSink) HandleUsage(context.Context, Record) {
	if s.count == 0 {
		close(s.entered)
		<-s.release
	}
	s.count++
}

func TestShutdownWaitsForPersistentSinks(t *testing.T) {
	m := NewManager(2)
	sink := &drainingTestSink{entered: make(chan struct{}), release: make(chan struct{})}
	m.Register(sink)
	m.Publish(t.Context(), Record{RequestID: "one"})
	m.Publish(t.Context(), Record{RequestID: "two"})
	<-sink.entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := m.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown = %v", err)
	}
	close(sink.release)
	if err := m.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if sink.count != 2 {
		t.Fatalf("Shutdown dropped queued events: %d", sink.count)
	}
}
