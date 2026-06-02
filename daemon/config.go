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

	Credentials CredentialsConfig
}

type CredentialsConfig struct {
	Type         string
	SpotifyToken SpotifyTokenCredentials
}

type SpotifyTokenCredentials struct {
	Username    string
	AccessToken string
}
