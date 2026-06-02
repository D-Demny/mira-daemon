package bluetooth

const (
	bluezBusName          = "org.bluez"
	bluezAdapterInterface = "org.bluez.Adapter1"
	bluezDeviceInterface  = "org.bluez.Device1"
	bluezAgentInterface   = "org.bluez.Agent1"
	bluezAgentManager     = "org.bluez.AgentManager1"
	bluezBatteryInterface = "org.bluez.Battery1"
	bluezNetworkInterface = "org.bluez.Network1"
	bluezObjectPath       = "/org/bluez"
	bluezAgentPath        = "/org/bluez/agent"

	panInterface = "bnep0"

	// panNAPUUID is the Bluetooth SIG service UUID for PAN Network Access Point
	// must be advertised by the phone for tethering to work
	panNAPUUID = "00001116-0000-1000-8000-00805f9b34fb"

	EventPairing            = "bluetooth/pairing"
	EventPairingCancelled   = "bluetooth/pairing/cancelled"
	EventPaired             = "bluetooth/paired"
	EventConnect            = "bluetooth/connect"
	EventDisconnect         = "bluetooth/disconnect"
	EventNetworkConnect     = "bluetooth/network/connect"
	EventNetworkDisconnect  = "bluetooth/network/disconnect"
	// EventNAPUnavailable fires when a paired device finishes service
	// discovery but never advertises the PAN-NAP UUID 
	EventNAPUnavailable     = "bluetooth/network/unavailable"
)
