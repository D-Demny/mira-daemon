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

	ReportURL string

	Checkin    bool
	CheckinURL string

	// on device voice service
	Voice VoiceConfig

	Credentials CredentialsConfig

	// Home Assistant REST API proxy target (epic 9): the browser cannot
	// reach HA cross-origin (no CORS for the UI origin), so the UI calls
	// /ha-api/... on the daemon API server instead. Empty URL = proxy off.
	HomeAssistant HomeAssistantConfig
}

type HomeAssistantConfig struct {
	URL   string
	Token string
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
