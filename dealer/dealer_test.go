package dealer

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/coder/websocket"
	librespot "github.com/devgianlu/go-librespot"
)

func TestPingTickerStopsWhenConnNil(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &Dealer{
			log:  &librespot.NullLogger{},
			done: make(chan struct{}),
		}
		stopped := make(chan struct{})

		go func() {
			defer close(stopped)
			d.pingTicker()
		}()

		time.Sleep(pingInterval + time.Nanosecond)
		synctest.Wait()

		d.Close()
		synctest.Wait()

		select {
		case <-stopped:
		default:
			t.Fatal("pingTicker did not stop")
		}
	})
}

func TestWriteConnRejectsClosedDealer(t *testing.T) {
	d := &Dealer{done: make(chan struct{})}
	close(d.done)

	_, err := d.writeConn(context.Background(), websocket.MessageText, nil)
	if !errors.Is(err, ErrDealerClosed) {
		t.Fatalf("expected ErrDealerClosed, got %v", err)
	}
}

func TestDeliverMessageDoesNotBlockOnFullReceiver(t *testing.T) {
	d := &Dealer{}
	recv := messageReceiver{c: make(chan Message, 1)}
	recv.c <- Message{Uri: "hm://connect-state/v1/old"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.deliverMessage(&librespot.NullLogger{}, recv, Message{Uri: "hm://connect-state/v1/new"})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deliverMessage blocked on a full receiver")
	}
}

func TestDeliverMessageMakesRoomForConnectionID(t *testing.T) {
	d := &Dealer{}
	recv := messageReceiver{c: make(chan Message, 1)}
	recv.c <- Message{Uri: "hm://connect-state/v1/old"}

	d.deliverMessage(&librespot.NullLogger{}, recv, Message{Uri: "hm://pusher/v1/connections/abc"})

	got := <-recv.c
	if got.Uri != "hm://pusher/v1/connections/abc" {
		t.Fatalf("buffered message = %q, want connection-id message", got.Uri)
	}
}
