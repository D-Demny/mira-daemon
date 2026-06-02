package daemon

import (
	librespot "github.com/devgianlu/go-librespot"
)

// Options bundles the dependencies a daemon needs at construction time.
type Options struct {
	Logger     librespot.Logger
	Config     *Config
	StateStore StateStore

	APIServer ApiServer
}
