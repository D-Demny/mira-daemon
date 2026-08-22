package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/devgianlu/go-librespot/daemon"
	"github.com/gofrs/flock"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/posflag"
	"github.com/knadh/koanf/v2"
	log "github.com/sirupsen/logrus"
	flag "github.com/spf13/pflag"
)

var errAlreadyRunning = errors.New("go-librespot is already running")

type cliConfig struct {
	ConfigDir string `koanf:"config_dir"`

	// Keep this around so the lockfile finalizer doesn't release it.
	configLock *flock.Flock

	LogLevel            log.Level `koanf:"log_level"`
	LogDisableTimestamp bool      `koanf:"log_disable_timestamp"`

	DeviceId    string `koanf:"device_id"`
	DeviceName  string `koanf:"device_name"`
	DeviceType  string `koanf:"device_type"`
	ClientToken string `koanf:"client_token"`

	ObserverMode bool `koanf:"observer_mode"`

	ReportURL string `koanf:"report_url"`

	Checkin    bool   `koanf:"checkin"`
	CheckinURL string `koanf:"checkin_url"`

	Server struct {
		Enabled     bool   `koanf:"enabled"`
		Address     string `koanf:"address"`
		Port        int    `koanf:"port"`
		AllowOrigin string `koanf:"allow_origin"`
		CertFile    string `koanf:"cert_file"`
		KeyFile     string `koanf:"key_file"`

		ImageSize string `koanf:"image_size"`
	} `koanf:"server"`

	Voice struct {
		Enabled              bool    `koanf:"enabled"`
		Wake                 bool    `koanf:"wake"`
		BinDir               string  `koanf:"bin_dir"`
		LibDir               string  `koanf:"lib_dir"`
		ModelDir             string  `koanf:"model_dir"`
		WakeThreshold        float64 `koanf:"wake_threshold"`
		WakeThresholdPlaying float64 `koanf:"wake_threshold_playing"`
		MicDevice            string  `koanf:"mic_device"`
		Cascade              bool    `koanf:"cascade"`
		EspeakBin            string  `koanf:"espeak_bin"`
		EspeakDataDir        string  `koanf:"espeak_data_dir"`
		CacheDir             string  `koanf:"cache_dir"`
		CatalogSync          bool    `koanf:"catalog_sync"`
		HashRotate           bool    `koanf:"hash_rotate"`
		AcceptThreshold      float64 `koanf:"accept_threshold"`
		SherpaEnabled        bool    `koanf:"sherpa_enabled"`
		SherpaBin            string  `koanf:"sherpa_bin"`
		SherpaModelDir       string  `koanf:"sherpa_model_dir"`
	} `koanf:"voice"`

	Credentials struct {
		Type         string `koanf:"type"`
		SpotifyToken struct {
			Username    string `koanf:"username"`
			AccessToken string `koanf:"access_token"`
		} `koanf:"spotify_token"`
	} `koanf:"credentials"`

	HomeAssistant struct {
		URL   string `koanf:"url"`
		Token string `koanf:"token"`
	} `koanf:"homeassistant"`
}

func (c *cliConfig) toDaemonConfig() *daemon.Config {
	dc := &daemon.Config{
		DeviceId:     c.DeviceId,
		DeviceName:   c.DeviceName,
		DeviceType:   c.DeviceType,
		ClientToken:  c.ClientToken,
		ObserverMode: c.ObserverMode,
		ReportURL:    c.ReportURL,
		Checkin:      c.Checkin,
		CheckinURL:   c.CheckinURL,
		ImageSize:    c.Server.ImageSize,
		Voice: daemon.VoiceConfig{
			Enabled:              c.Voice.Enabled,
			Wake:                 c.Voice.Wake,
			BinDir:               c.Voice.BinDir,
			LibDir:               c.Voice.LibDir,
			ModelDir:             c.Voice.ModelDir,
			WakeThreshold:        c.Voice.WakeThreshold,
			WakeThresholdPlaying: c.Voice.WakeThresholdPlaying,
			MicDevice:            c.Voice.MicDevice,
			Cascade:              c.Voice.Cascade,
			EspeakBin:            c.Voice.EspeakBin,
			EspeakDataDir:        c.Voice.EspeakDataDir,
			CacheDir:             c.Voice.CacheDir,
			CatalogSync:          c.Voice.CatalogSync,
			HashRotate:           c.Voice.HashRotate,
			AcceptThreshold:      c.Voice.AcceptThreshold,
			SherpaEnabled:        c.Voice.SherpaEnabled,
			SherpaBin:            c.Voice.SherpaBin,
			SherpaModelDir:       c.Voice.SherpaModelDir,
		},
	}
	dc.Credentials.Type = c.Credentials.Type
	dc.Credentials.SpotifyToken.Username = c.Credentials.SpotifyToken.Username
	dc.Credentials.SpotifyToken.AccessToken = c.Credentials.SpotifyToken.AccessToken
	dc.HomeAssistant.URL = c.HomeAssistant.URL
	dc.HomeAssistant.Token = c.HomeAssistant.Token
	return dc
}

func loadCLIConfig(cfg *cliConfig) error {
	f := flag.NewFlagSet("config", flag.ContinueOnError)
	f.Usage = func() {
		fmt.Println(f.FlagUsages())
		os.Exit(0)
	}
	userConfigDir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	defaultConfigDir := filepath.Join(userConfigDir, "go-librespot")
	f.StringVar(&cfg.ConfigDir, "config_dir", defaultConfigDir, "the configuration directory")

	var configOverrides []string
	f.StringArrayVarP(&configOverrides, "conf", "c", nil, "override config values (format: field=value, use field1.field2=value for nested fields)")

	if err := f.Parse(os.Args[1:]); err != nil {
		return err
	}

	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		return fmt.Errorf("failed creating config directory: %w", err)
	}

	lockFilePath := filepath.Join(cfg.ConfigDir, "lockfile")
	cfg.configLock = flock.New(lockFilePath)
	if locked, err := cfg.configLock.TryLock(); err != nil {
		return fmt.Errorf("could not lock config directory: %w", err)
	} else if !locked {
		return fmt.Errorf("%w (lockfile: %s)", errAlreadyRunning, lockFilePath)
	}

	k := koanf.New(".")

	_ = k.Load(confmap.Provider(map[string]interface{}{
		"log_level": log.InfoLevel,

		"device_type": "computer",

		"credentials.type": "interactive",

		"checkin":     true,
		"checkin_url": "https://mira-checkin.mira-thing.workers.dev",

		"server.address":    "localhost",
		"server.image_size": "default",

		"voice.bin_dir":                "/opt/voice/bin",
		"voice.lib_dir":                "/opt/voice/lib",
		"voice.model_dir":              "/opt/voice/models",
		"voice.wake_threshold":         0.4,
		"voice.wake_threshold_playing": 0.6,
		"voice.mic_device":             "hw:0,0",
		"voice.cascade":                true,
		"voice.espeak_bin":             "espeak-ng",
		"voice.catalog_sync":           true,
		"voice.hash_rotate":            true,
		"voice.sherpa_bin":             "sherpa_asr_server",
	}, "."), nil)

	var configPath string
	if _, err := os.Stat(filepath.Join(cfg.ConfigDir, "config.yaml")); os.IsNotExist(err) {
		configPath = filepath.Join(cfg.ConfigDir, "config.yml")
	} else {
		configPath = filepath.Join(cfg.ConfigDir, "config.yaml")
	}

	// A corrupt config file must not crash loop daemon
	quarantineConfig := func(reason error) error {
		target := configPath + ".corrupt"
		if renameErr := os.Rename(configPath, target); renameErr != nil {
			return fmt.Errorf("failed reading configuration file: %w", reason)
		}
		return fmt.Errorf("configuration file unusable, quarantined to %s: %w", target, reason)
	}

	configFileLoaded := false
	if err := k.Load(file.Provider(configPath), yaml.Parser()); err != nil {
		if !os.IsNotExist(err) {
			return quarantineConfig(err)
		}
	} else {
		configFileLoaded = true
	}

	if err := k.Load(posflag.Provider(f, ".", k), nil); err != nil {
		return fmt.Errorf("failed loading command line configuration: %w", err)
	}

	if len(configOverrides) > 0 {
		overrideMap := make(map[string]interface{})
		for _, override := range configOverrides {
			parts := strings.SplitN(override, "=", 2)
			if len(parts) != 2 {
				return fmt.Errorf("invalid config override format: %s (expected field=value)", override)
			}
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			if key == "" {
				return fmt.Errorf("invalid config override: empty field name in %s", override)
			}
			overrideMap[key] = value
		}
		if err := k.Load(confmap.Provider(overrideMap, "."), nil); err != nil {
			return fmt.Errorf("failed loading config overrides: %w", err)
		}
	}

	if err := k.Unmarshal("", &cfg); err != nil {
		if configFileLoaded {
			return quarantineConfig(fmt.Errorf("failed to unmarshal configuration: %w", err))
		}
		return fmt.Errorf("failed to unmarshal configuration: %w", err)
	}

	if cfg.DeviceName == "" {
		cfg.DeviceName = "go-librespot"

		hostname, _ := os.Hostname()
		if hostname != "" {
			cfg.DeviceName += " " + hostname
		}
	}

	return nil
}
