package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	librespot "github.com/devgianlu/go-librespot"
)

// per-client-writer design
// Emit must never block the caller
func TestEmitDoesNotBlockOnFullClientBuffer(t *testing.T) {
	s := &ConcreteApiServer{log: &librespot.NullLogger{}}

	// a client whose buffer is full with no writer goroutine draining it
	stuck := &wsClient{send: make(chan *ApiEvent, 1)}
	stuck.send <- &ApiEvent{Type: ApiEventTypePlaying}
	s.clients = []*wsClient{stuck}

	done := make(chan struct{})
	go func() {
		s.Emit(&ApiEvent{Type: ApiEventTypePaused})
		close(done)
	}()

	select {
	case <-done:
		// good, emit returned without blocking
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked on a full client buffer")
	}

	// the event was dropped, not queued
	if got := len(stuck.send); got != 1 {
		t.Fatalf("expected event dropped (buffer len 1), got %d", got)
	}
}

func TestEmitEnqueuesToClientWithCapacity(t *testing.T) {
	s := &ConcreteApiServer{log: &librespot.NullLogger{}}
	c := &wsClient{send: make(chan *ApiEvent, 4)}
	s.clients = []*wsClient{c}

	s.Emit(&ApiEvent{Type: ApiEventTypePlaying})

	if got := len(c.send); got != 1 {
		t.Fatalf("expected 1 buffered event, got %d", got)
	}
	if ev := <-c.send; ev.Type != ApiEventTypePlaying {
		t.Fatalf("buffered event type = %q, want %q", ev.Type, ApiEventTypePlaying)
	}
}

// End-to-end over a real socket
func TestEventsWebsocketDeliversAndCleansUp(t *testing.T) {
	srvIface, err := NewApiServer(&librespot.NullLogger{}, "127.0.0.1", 0, "", "", "")
	if err != nil {
		t.Fatalf("NewApiServer: %v", err)
	}
	srv := srvIface.(*ConcreteApiServer)
	defer func() { _ = srv.Close() }()

	wsURL := "ws://" + srv.listener.Addr().String() + "/events"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial /events: %v", err)
	}

	waitForClientCount(t, srv, 1)

	srv.Emit(&ApiEvent{Type: ApiEventTypePlaying})

	var got ApiEvent
	if err := wsjson.Read(ctx, conn, &got); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if got.Type != ApiEventTypePlaying {
		t.Fatalf("event type = %q, want %q", got.Type, ApiEventTypePlaying)
	}

	// client disconnect -> server's read loop drops it
	_ = conn.Close(websocket.StatusNormalClosure, "")
	waitForClientCount(t, srv, 0)
}

func waitForClientCount(t *testing.T, s *ConcreteApiServer, want int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		s.clientsLock.RLock()
		n := len(s.clients)
		s.clientsLock.RUnlock()
		if n == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("client count never reached %d", want)
}
