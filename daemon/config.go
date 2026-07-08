package daemon

// Config carries the runtime configuration for a daemon instance.
type Config struct {
	DeviceId    string
	DeviceName  string
	DeviceType  string
	ClientToken string

	// ObserverMode when true prevents this device from becoming the active playback device
	ObserverMode bool

	// ImageSize selects which cover-art image variant the API server returns:
	// "default", "small", "medium", "large", "xlarge".
	ImageSize string

	// on device voice service
	Voice VoiceConfig

	Credentials CredentialsConfig
}

// configures the on device voice service
type VoiceConfig struct {
	// starts the voice service
	Enabled bool
	// spawns the always on wake-word listener
	Wake bool

	BinDir   string
	LibDir   string
	ModelDir string

	WakeThreshold float64
	// stricter wake threshold when music is playing
	WakeThresholdPlaying float64
	MicDevice            string

	Cascade         bool
	EspeakBin       string
	EspeakDataDir   string
	CacheDir        string
	CatalogSync     bool
	HashRotate      bool
	AcceptThreshold float64

	SherpaEnabled  bool
	SherpaBin      string
	SherpaModelDir string
}

type CredentialsConfig struct {
	Type         string
	SpotifyToken SpotifyTokenCredentials
}

type SpotifyTokenCredentials struct {
	Username    string
	AccessToken string
}
