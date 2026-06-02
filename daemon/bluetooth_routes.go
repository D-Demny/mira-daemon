package daemon

import (
	"encoding/json"
	"net/http"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/daemon/bluetooth"
)

// BluetoothHandler is the subset of bluetooth.Manager that the HTTP layer needs
type BluetoothHandler interface {
	SetDiscoverable(enable bool) error
	GetDevices() ([]bluetooth.DeviceInfo, error)
	GetDeviceInfo(address string) (*bluetooth.DeviceInfo, error)
	RemoveDevice(address string) error
	ConnectDevice(address string) error
	DisconnectDevice(address string) error
	ConnectNetwork(address string) error
	NetworkUp() bool
	AcceptPairing() error
	DenyPairing() error
	GetCurrentPairingRequest() *bluetooth.PairingRequest
}

func writeBluetoothJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeBluetoothError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func bluetoothUnavailable(w http.ResponseWriter) {
	w.WriteHeader(http.StatusServiceUnavailable)
}

func registerBluetoothRoutes(log librespot.Logger, m *http.ServeMux, get func() BluetoothHandler) {
	// trace wraps a handler with entry/exit logging so we can see in the log exactly where a request gets stuck
	trace := func(fn http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			log.Debugf("http: %s %s entry", r.Method, r.URL.Path)
			fn(w, r)
			log.Debugf("http: %s %s exit", r.Method, r.URL.Path)
		}
	}

	m.HandleFunc("POST /bluetooth/discover/on", trace(func(w http.ResponseWriter, r *http.Request) {
		bm := get()
		if bm == nil {
			bluetoothUnavailable(w)
			return
		}
		if err := bm.SetDiscoverable(true); err != nil {
			writeBluetoothError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	m.HandleFunc("POST /bluetooth/discover/off", trace(func(w http.ResponseWriter, r *http.Request) {
		bm := get()
		if bm == nil {
			bluetoothUnavailable(w)
			return
		}
		if err := bm.SetDiscoverable(false); err != nil {
			writeBluetoothError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	m.HandleFunc("GET /bluetooth/devices", trace(func(w http.ResponseWriter, r *http.Request) {
		bm := get()
		if bm == nil {
			bluetoothUnavailable(w)
			return
		}
		devices, err := bm.GetDevices()
		if err != nil {
			writeBluetoothError(w, http.StatusInternalServerError, err)
			return
		}
		if devices == nil {
			devices = []bluetooth.DeviceInfo{}
		}
		writeBluetoothJSON(w, devices)
	}))

	m.HandleFunc("GET /bluetooth/info/{addr}", trace(func(w http.ResponseWriter, r *http.Request) {
		bm := get()
		if bm == nil {
			bluetoothUnavailable(w)
			return
		}
		info, err := bm.GetDeviceInfo(r.PathValue("addr"))
		if err != nil {
			writeBluetoothError(w, http.StatusNotFound, err)
			return
		}
		writeBluetoothJSON(w, info)
	}))

	m.HandleFunc("POST /bluetooth/remove/{addr}", trace(func(w http.ResponseWriter, r *http.Request) {
		bm := get()
		if bm == nil {
			bluetoothUnavailable(w)
			return
		}
		if err := bm.RemoveDevice(r.PathValue("addr")); err != nil {
			writeBluetoothError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	m.HandleFunc("POST /bluetooth/connect/{addr}", trace(func(w http.ResponseWriter, r *http.Request) {
		bm := get()
		if bm == nil {
			bluetoothUnavailable(w)
			return
		}
		if err := bm.ConnectDevice(r.PathValue("addr")); err != nil {
			writeBluetoothError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	m.HandleFunc("POST /bluetooth/disconnect/{addr}", trace(func(w http.ResponseWriter, r *http.Request) {
		bm := get()
		if bm == nil {
			bluetoothUnavailable(w)
			return
		}
		if err := bm.DisconnectDevice(r.PathValue("addr")); err != nil {
			writeBluetoothError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	m.HandleFunc("POST /bluetooth/network/{addr}", trace(func(w http.ResponseWriter, r *http.Request) {
		bm := get()
		if bm == nil {
			bluetoothUnavailable(w)
			return
		}
		if err := bm.ConnectNetwork(r.PathValue("addr")); err != nil {
			writeBluetoothError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	m.HandleFunc("GET /bluetooth/network", trace(func(w http.ResponseWriter, r *http.Request) {
		bm := get()
		if bm == nil {
			bluetoothUnavailable(w)
			return
		}
		writeBluetoothJSON(w, map[string]bool{"up": bm.NetworkUp()})
	}))

	m.HandleFunc("GET /bluetooth/pairing", trace(func(w http.ResponseWriter, r *http.Request) {
		bm := get()
		if bm == nil {
			bluetoothUnavailable(w)
			return
		}
		req := bm.GetCurrentPairingRequest()
		if req == nil {
			writeBluetoothJSON(w, map[string]any{"pending": false})
			return
		}
		writeBluetoothJSON(w, map[string]any{
			"pending": true,
			"request": req,
		})
	}))

	m.HandleFunc("POST /bluetooth/pairing/accept", trace(func(w http.ResponseWriter, r *http.Request) {
		bm := get()
		if bm == nil {
			bluetoothUnavailable(w)
			return
		}
		if err := bm.AcceptPairing(); err != nil {
			writeBluetoothError(w, http.StatusBadRequest, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	m.HandleFunc("POST /bluetooth/pairing/deny", trace(func(w http.ResponseWriter, r *http.Request) {
		bm := get()
		if bm == nil {
			bluetoothUnavailable(w)
			return
		}
		if err := bm.DenyPairing(); err != nil {
			writeBluetoothError(w, http.StatusBadRequest, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
}
