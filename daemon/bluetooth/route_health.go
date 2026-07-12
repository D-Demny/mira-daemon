package bluetooth

import (
	"context"
	"net"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const (
	probeInterval   = 20 * time.Second
	probeTimeout    = 4 * time.Second
	demoteAfter     = 3
	restoreAfter    = 3
	restoreHoldDown = 5 * time.Minute
	demotedMetric   = 700
	probeTarget     = "1.1.1.1:443"
)

type routeHealthState struct {
	failStreak int
	okStreak   int
	demoted    bool
	demotedAt  time.Time
}

type routeAction int

const (
	actionNone routeAction = iota
	actionDemote
	actionRestore
)

// feeds one probe round into the state machine
func (s *routeHealthState) tick(panOK, altOK bool, now time.Time) routeAction {
	if panOK {
		s.failStreak = 0
		s.okStreak++
		if s.demoted && s.okStreak >= restoreAfter && now.Sub(s.demotedAt) >= restoreHoldDown {
			s.demoted = false
			s.okStreak = 0
			return actionRestore
		}
		return actionNone
	}
	s.okStreak = 0
	if !altOK {
		s.failStreak = 0
		return actionNone
	}
	s.failStreak++
	if !s.demoted && s.failStreak >= demoteAfter {
		s.demoted = true
		s.demotedAt = now
		s.failStreak = 0
		return actionDemote
	}
	return actionNone
}

func (s *routeHealthState) reset() {
	*s = routeHealthState{}
}

func probeVia(iface string) bool {
	d := net.Dialer{
		Timeout: probeTimeout,
		Control: func(network, address string, c syscall.RawConn) error {
			var bindErr error
			if err := c.Control(func(fd uintptr) {
				bindErr = unix.BindToDevice(int(fd), iface)
			}); err != nil {
				return err
			}
			return bindErr
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", probeTarget)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// returns bnep0s default route or nil
func panDefaultRoute() *netlink.Route {
	link, err := netlink.LinkByName(panInterface)
	if err != nil {
		return nil
	}
	routes, err := netlink.RouteList(link, netlink.FAMILY_V4)
	if err != nil {
		return nil
	}
	for i := range routes {
		if routes[i].Dst == nil && routes[i].Gw != nil {
			return &routes[i]
		}
	}
	return nil
}

func otherDefaultRouteIface() string {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return ""
	}
	for _, r := range routes {
		if r.Dst != nil || r.Gw == nil {
			continue
		}
		link, err := netlink.LinkByIndex(r.LinkIndex)
		if err != nil || link.Attrs().Name == panInterface {
			continue
		}
		return link.Attrs().Name
	}
	return ""
}

// replaces bnep0 default route with the given metric
func (m *Manager) setPanRouteMetric(metric int) error {
	route := panDefaultRoute()
	if route == nil {
		return nil
	}
	if route.Priority == metric {
		return nil
	}
	newRoute := *route
	newRoute.Priority = metric
	if err := netlink.RouteAdd(&newRoute); err != nil && err != unix.EEXIST {
		return err
	}
	return netlink.RouteDel(route)
}

func (m *Manager) routeArbiterLoop() {
	var state routeHealthState
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()

	for range ticker.C {
		route := panDefaultRoute()
		if route == nil {
			state.reset()
			continue
		}
		if state.demoted && route.Priority == 0 {
			state.reset()
		}

		alt := otherDefaultRouteIface()
		altOK := alt != "" && probeVia(alt)
		panOK := probeVia(panInterface)

		switch state.tick(panOK, altOK, time.Now()) {
		case actionDemote:
			m.log.Warnf("bluetooth: %s default route unhealthy (TCP dead %d probes, %s healthy), demoting to metric %d",
				panInterface, demoteAfter, alt, demotedMetric)
			if err := m.setPanRouteMetric(demotedMetric); err != nil {
				m.log.WithError(err).Warnf("bluetooth: failed to demote %s route", panInterface)
				state.reset() // try again next round
			}
		case actionRestore:
			m.log.Infof("bluetooth: %s recovered (%d clean probes, %s hold-down), restoring primary route",
				panInterface, restoreAfter, restoreHoldDown)
			if err := m.setPanRouteMetric(0); err != nil {
				m.log.WithError(err).Warnf("bluetooth: failed to restore %s route", panInterface)
			}
		}
	}
}
