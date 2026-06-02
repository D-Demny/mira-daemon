package bluetooth

type DeviceInfo struct {
	Address           string `json:"address"`
	Name              string `json:"name"`
	Alias             string `json:"alias"`
	Class             string `json:"class"`
	Icon              string `json:"icon"`
	Paired            bool   `json:"paired"`
	Trusted           bool   `json:"trusted"`
	Blocked           bool   `json:"blocked"`
	Connected         bool   `json:"connected"`
	LegacyPairing     bool   `json:"legacyPairing"`
	BatteryPercentage int    `json:"batteryPercentage,omitempty"`
}

type PairingRequest struct {
	Device      string `json:"device"`
	Passkey     string `json:"passkey"`
	RequestType string `json:"requestType"`
}

type PairingStartedPayload struct {
	Address    string `json:"address"`
	PairingKey string `json:"pairingKey"`
}

type DevicePairedPayload struct {
	Device *DeviceInfo `json:"device"`
}

type DeviceConnectedPayload struct {
	Address string      `json:"address"`
	Device  *DeviceInfo `json:"device,omitempty"`
}

type DeviceDisconnectedPayload struct {
	Address string `json:"address"`
}

type NetworkConnectedPayload struct {
	Address string `json:"address"`
}

// Emitter forwards bluetooth events to the daemon's event bus
type Emitter func(eventType string, payload any)
