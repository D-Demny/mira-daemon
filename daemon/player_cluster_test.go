package daemon

import (
	"testing"

	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
)

// injectCluster hands the put_state-response cluster
func TestInjectCluster_NewestWinsAndNeverBlocks(t *testing.T) {
	p := &AppPlayer{clusterCh: make(chan *connectpb.Cluster, 1)}

	first := &connectpb.Cluster{ActiveDeviceId: "first"}
	second := &connectpb.Cluster{ActiveDeviceId: "second"}

	// fills the buffer
	p.injectCluster(first)
	// must not block even though the buffer is full, and must replace
	p.injectCluster(second)

	select {
	case got := <-p.clusterCh:
		if got.ActiveDeviceId != "second" {
			t.Fatalf("newest snapshot should win, got %q", got.ActiveDeviceId)
		}
	default:
		t.Fatal("expected a queued cluster")
	}

	// channel should now be empty
	select {
	case extra := <-p.clusterCh:
		t.Fatalf("expected empty channel, got %q", extra.ActiveDeviceId)
	default:
	}

	// a nil cluster is a no-op
	p.injectCluster(nil)
	select {
	case <-p.clusterCh:
		t.Fatal("nil cluster should not be queued")
	default:
	}
}
