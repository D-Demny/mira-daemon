package daemon

import (
	"context"
	"net"
	"os/exec"
	"time"

	"github.com/vishvananda/netlink"

	librespot "github.com/devgianlu/go-librespot"
)

// pinged once a second to test internet reachability
const networkMonitorTarget = "1.1.1.1"

// linkUp reports whether a network interface exists and is up
func linkUp(name string) bool {
	link, err := netlink.LinkByName(name)
	return err == nil && link.Attrs().Flags&net.FlagUp != 0
}

// startNetworkMonitor pings + emits network_status on transition
func startNetworkMonitor(log librespot.Logger, server ApiServer, onTransition func(online bool)) {
	const (
		interval       = 1 * time.Second
		failThreshold  = 3
		rebroadcastSec = 15
	)

	go func() {
		var (
			online        bool
			failCount     int
			emitted       bool
			tickSinceEmit int
		)

		ping := func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "ping", "-c", "1", "-W", "1", networkMonitorTarget)
			return cmd.Run() == nil
		}

		confirm := func() bool {
			conn, err := net.DialTimeout("tcp", networkMonitorTarget+":443", 1200*time.Millisecond)
			if err != nil {
				return false
			}
			_ = conn.Close()
			return true
		}

		emit := func(status string) {
			server.Emit(&ApiEvent{
				Type: ApiEventTypeNetworkStatus,
				Data: map[string]any{
					"status": status,
					"usb":    linkUp("usb0"),
					"bt":     linkUp("bnep0"),
				},
			})
			tickSinceEmit = 0
		}

		for {
			ok := ping()
			if !ok {
				ok = confirm()
			}
			if ok {
				failCount = 0
				if !online || !emitted {
					online = true
					emitted = true
					log.Info("network: online")
					emit("online")
					if onTransition != nil {
						onTransition(true)
					}
				}
			} else {
				failCount++
				if failCount >= failThreshold && (online || !emitted) {
					online = false
					emitted = true
					log.Warn("network: offline")
					emit("offline")
					if onTransition != nil {
						onTransition(false)
					}
				}
			}

			// re-broadcast for late WS clients, only once we've determined a state
			if emitted {
				tickSinceEmit++
				if tickSinceEmit >= rebroadcastSec {
					if online {
						emit("online")
					} else {
						emit("offline")
					}
				}
			}

			time.Sleep(interval)
		}
	}()
}
