package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	librespot "github.com/devgianlu/go-librespot"
	"github.com/devgianlu/go-librespot/ma"
)

// issue #134 (daemon part 2): unit tests for the MA observer mapping,
// settings-blob gating, device list / player selection endpoints and the
// control-verb routing.

// newMaTestPlayer wires a no-op API server + a mutable settings blob so
// maModeActive()/maResolved() see what the tests program (same pattern as
// connect_devices_test.go).
func newMaTestPlayer(t *testing.T, blob []byte) *AppPlayer {
	t.Helper()
	stub, err := NewStubApiServer(&librespot.NullLogger{})
	if err != nil {
		t.Fatalf("stub api server: %v", err)
	}
	state := &librespot.AppState{}
	if blob != nil {
		state.Settings = blob
	}
	return &AppPlayer{
		app:   &App{log: &librespot.NullLogger{}, state: state, server: stub, deviceId: "self"},
		state: &State{},
	}
}

func maTestSettings(url, src string) []byte {
	return []byte(fmt.Sprintf(`{"ma":{"url":%q,"token":"tok"},"playbackSource":%q}`, url, src))
}

// TestMaVolumeMapping pins both directions of the 0..100 <-> 0..MaxStateVolume
// scale conversion.
func TestMaVolumeMapping(t *testing.T) {
	t.Parallel()

	volCases := []struct {
		in   *int
		want uint32
	}{
		{nil, 0},
		{new(int(-5)), 0},
		{new(int(0)), 0},
		{new(int(50)), 50 * MaxStateVolume / 100},
		{new(int(80)), 80 * MaxStateVolume / 100},
		{new(int(100)), MaxStateVolume},
		{new(int(150)), MaxStateVolume}, // clamped
	}
	for _, tc := range volCases {
		if got := maVolumeToState(tc.in); got != tc.want {
			t.Errorf("maVolumeToState(%v) = %d want %d", tc.in, got, tc.want)
		}
	}

	levCases := []struct {
		in   int64
		want int
	}{
		{0, 0},
		{MaxStateVolume / 2, (MaxStateVolume / 2) * 100 / MaxStateVolume},
		{MaxStateVolume, 100},
	}
	for _, tc := range levCases {
		if got := maTargetToLevel(tc.in); got != tc.want {
			t.Errorf("maTargetToLevel(%d) = %d want %d", tc.in, got, tc.want)
		}
	}
}

// TestMaModeActive_GatingOnSettingsBlob: the observer switch is exactly
// "usable ma config AND playbackSource == ma" (issue #134 contract).
func TestMaModeActive_GatingOnSettingsBlob(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		blob []byte
		want bool
	}{
		{"ma config + source ma", maTestSettings("http://h:8095", "ma"), true},
		{"ma config + source spotify", maTestSettings("http://h:8095", "spotify"), false},
		{"ma config + source absent (old UI)", []byte(`{"ma":{"url":"http://h:8095","token":"t"}}`), false},
		{"unusable url + source ma", maTestSettings("", "ma"), false},
		{"no ma key at all", []byte(`{"volume":40}`), false},
		{"empty blob", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newMaTestPlayer(t, tc.blob)
			if got := p.maModeActive(); got != tc.want {
				t.Errorf("maModeActive() = %v want %v", got, tc.want)
			}
		})
	}
}

func maPlayingPlayer() *ma.Player {
	return &ma.Player{
		PlayerID:               "wiim_uuid:A",
		Name:                   "WiiM Wohnzimmer",
		Available:              true,
		PlaybackState:          "playing",
		ElapsedTime:            new(float64(42.5)),
		ElapsedTimeLastUpdated: new(float64(1700000000)),
		VolumeLevel:            new(int(80)),
		CurrentMedia: &ma.CurrentMedia{
			URI:      "spotify--x://track/abc",
			Title:    "Glide",
			Artist:   "NEIKED/Portugal. The Man",
			Album:    "The World Is Yours",
			ImageURL: "http://10.10.1.104:8095/imageproxy/x?size=512&fmt=jpg",
			Duration: 184,
		},
	}
}

// TestApplyMaState_Playing maps a polled MA player into the shared
// RemoteState with the exact shapes /observer/status and the UI consume.
func TestApplyMaState_Playing(t *testing.T) {
	t.Parallel()
	p := newMaTestPlayer(t, nil)
	pl := maPlayingPlayer()

	p.applyMaState(pl)

	rs := p.state.remoteState
	if rs == nil {
		t.Fatal("remoteState must be set")
	}
	if rs.DeviceId != "wiim_uuid:A" || rs.DeviceName != "WiiM Wohnzimmer" || rs.DeviceType != maDeviceType {
		t.Errorf("device identity: %+v", rs)
	}
	if rs.TrackUri != pl.CurrentMedia.URI || rs.TrackName != "Glide" ||
		rs.TrackArtist != "NEIKED/Portugal. The Man" || rs.TrackAlbum != "The World Is Yours" ||
		rs.TrackImageUrl != pl.CurrentMedia.ImageURL {
		t.Errorf("track fields: %+v", rs)
	}
	if rs.Duration != 184000 { // seconds -> ms
		t.Errorf("duration: got %d want 184000", rs.Duration)
	}
	if !rs.IsPlaying || rs.IsPaused || rs.PlaybackSpeed != 1 {
		t.Errorf("playback flags: %+v", rs)
	}
	if rs.Volume != uint32(80*MaxStateVolume/100) || rs.VolumeSteps != 100 {
		t.Errorf("volume: got %d/%d want %d/100", rs.Volume, rs.VolumeSteps, 80*MaxStateVolume/100)
	}
	if rs.PositionAsOfTimestamp != 42500 {
		t.Errorf("position as of: got %d want 42500 (seconds -> ms)", rs.PositionAsOfTimestamp)
	}
	if rs.Timestamp != 1700000000000 { // epoch seconds -> unix ms
		t.Errorf("timestamp: got %d want 1700000000000", rs.Timestamp)
	}
	if !p.maOwnsRemoteState {
		t.Error("maOwnsRemoteState must be set")
	}
	if p.state.lastActiveDeviceId != "wiim_uuid:A" || p.state.lastActiveDeviceName != "WiiM Wohnzimmer" {
		t.Errorf("last active device: %q/%q", p.state.lastActiveDeviceId, p.state.lastActiveDeviceName)
	}
}

// TestApplyMaState_Paused keeps the position but flags paused state.
func TestApplyMaState_Paused(t *testing.T) {
	t.Parallel()
	p := newMaTestPlayer(t, nil)
	pl := maPlayingPlayer()
	pl.PlaybackState = "paused"

	p.applyMaState(pl)

	rs := p.state.remoteState
	if rs == nil {
		t.Fatal("remoteState must be set")
	}
	if rs.IsPlaying || !rs.IsPaused || rs.PlaybackSpeed != 0 {
		t.Errorf("paused flags: %+v", rs)
	}
}

// TestApplyMaState_IdleClearsStaleMedia: live MA players keep a non-null
// (stale) current_media after going idle, so PlaybackState gates first;
// an idle poll must clear the observed state and release ownership.
func TestApplyMaState_IdleClearsStaleMedia(t *testing.T) {
	t.Parallel()
	p := newMaTestPlayer(t, nil)
	p.applyMaState(maPlayingPlayer()) // seed a playing state
	if p.state.remoteState == nil {
		t.Fatal("seed failed")
	}

	pl := maPlayingPlayer()
	pl.PlaybackState = "idle" // media intentionally still set (stale)
	p.applyMaState(pl)

	if p.state.remoteState != nil {
		t.Error("idle poll must clear remoteState")
	}
	if p.maOwnsRemoteState {
		t.Error("ownership must be released on idle")
	}
}

// TestApplyMaState_NullMediaClears: no media at all is also an idle signal.
func TestApplyMaState_NullMediaClears(t *testing.T) {
	t.Parallel()
	p := newMaTestPlayer(t, nil)
	pl := maPlayingPlayer()
	pl.CurrentMedia = nil

	p.applyMaState(pl)
	if p.state.remoteState != nil {
		t.Error("null-media poll must clear remoteState")
	}
}

// TestMaPollTick_ReleasesStateWhenModeOff: switching playbackSource back
// to spotify (or removing the MA config) must hand remoteState ownership
// back to the cluster path on the next tick.
func TestMaPollTick_ReleasesStateWhenModeOff(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"source switched to spotify", "ma config removed"} {
		t.Run(name, func(t *testing.T) {
			p := newMaTestPlayer(t, maTestSettings("http://h:8095", "ma"))
			p.applyMaState(maPlayingPlayer())
			if p.state.remoteState == nil {
				t.Fatal("seed failed")
			}

			if name == "source switched to spotify" {
				p.app.state.Settings = maTestSettings("http://h:8095", "spotify")
			} else {
				p.app.state.Settings = []byte(`{"playbackSource":"ma"}`)
			}

			p.maPollTick(context.Background())

			if p.state.remoteState != nil || p.maOwnsRemoteState {
				t.Errorf("state must be released, got %+v / owns=%v", p.state.remoteState, p.maOwnsRemoteState)
			}
		})
	}
}

// TestMaPollTick_NoSelectionReleasesForeignState: MA mode is the single
// source of truth - with no player selected, a stale session observed from
// another source (a Spotify session active before the switch) must be
// released on the next tick so the UI falls back to the idle screen with
// the MA picker instead of staying on the now-playing screen with the
// Spotify device list. The old ownership-only gate kept exactly that stale
// state visible indefinitely (issue #134 follow-up).
func TestMaPollTick_NoSelectionReleasesForeignState(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		settings func() []byte
		owns     bool
		wantGone bool
	}{
		{"MA mode releases spotify-owned state", func() []byte { return maTestSettings("http://h:8095", "ma") }, false, true},
		{"MA mode releases MA-owned state too", func() []byte { return maTestSettings("http://h:8095", "ma") }, true, true},
		{"spotify mode keeps cluster-owned state", func() []byte { return maTestSettings("http://h:8095", "spotify") }, false, false},
		{"spotify mode releases MA-owned state", func() []byte { return maTestSettings("http://h:8095", "spotify") }, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newMaTestPlayer(t, tc.settings())
			prev := &RemoteState{DeviceId: "spotify-dev-1", TrackUri: "spotify:track:x"}
			p.state.remoteState = prev // cluster-owned (or MA-owned per case)
			p.maOwnsRemoteState = tc.owns

			p.maPollTick(context.Background())

			if tc.wantGone {
				if p.state.remoteState != nil {
					t.Errorf("state must be released, got %+v", p.state.remoteState)
				}
				if p.maOwnsRemoteState {
					t.Error("ownership must be cleared")
				}
			} else if p.state.remoteState != prev {
				t.Error("tick must leave previous state untouched")
			}
		})
	}
}

const maTestPlayersJSON = `[
  {"player_id": "wiim_uuid:A", "name": "WiiM Wohnzimmer", "available": true, "playback_state": "playing"},
  {"player_id": "upfac71c9e", "name": "Unpowered", "available": false, "playback_state": "idle"}
]`

func newMaFakeServer(t *testing.T) (*httptest.Server, *map[string]any) {
	t.Helper()
	var last map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var env map[string]any
		_ = json.Unmarshal(b, &env)
		last = env
		if env["command"] == "players/all" {
			_, _ = w.Write([]byte(maTestPlayersJSON))
			return
		}
		_, _ = w.Write([]byte(`null`))
	}))
	t.Cleanup(srv.Close)
	return srv, &last
}

func devicesFromPayload(t *testing.T, payload any) []MaDevice {
	t.Helper()
	m, ok := payload.(map[string]any)
	if !ok {
		t.Fatalf("payload must be an object: %T", payload)
	}
	raw, ok := m["devices"].([]MaDevice)
	if !ok {
		// re-marshal round trip to tolerate typed vs generic slices
		data, _ := json.Marshal(m["devices"])
		var typed []MaDevice
		if err := json.Unmarshal(data, &typed); err != nil {
			t.Fatalf("devices field: %v", err)
		}
		return typed
	}
	return raw
}

// TestMaListDevices pins the GET /ma/devices contract shape.
func TestMaListDevices(t *testing.T) {
	t.Parallel()

	t.Run("lists players from players/all", func(t *testing.T) {
		srv, _ := newMaFakeServer(t)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))

		payload, err := p.maListDevices(context.Background())
		if err != nil {
			t.Fatalf("maListDevices: %v", err)
		}
		devs := devicesFromPayload(t, payload)
		if len(devs) != 2 {
			t.Fatalf("want 2 devices, got %d", len(devs))
		}
		if devs[0].Id != "wiim_uuid:A" || devs[0].Name != "WiiM Wohnzimmer" || !devs[0].Available {
			t.Errorf("device 0: %+v", devs[0])
		}
		if devs[1].Id != "upfac71c9e" || devs[1].Available {
			t.Errorf("device 1: %+v", devs[1])
		}
	})

	t.Run("unreachable server yields empty list, no error", func(t *testing.T) {
		srv, _ := newMaFakeServer(t)
		url := srv.URL
		srv.Close() // point the config at a dead port

		p := newMaTestPlayer(t, maTestSettings(url, "ma"))
		payload, err := p.maListDevices(context.Background())
		if err != nil {
			t.Fatalf("unreachable must not error: %v", err)
		}
		if devs := devicesFromPayload(t, payload); len(devs) != 0 {
			t.Errorf("want empty list, got %+v", devs)
		}
	})

	t.Run("unconfigured yields empty list", func(t *testing.T) {
		p := newMaTestPlayer(t, nil)
		payload, err := p.maListDevices(context.Background())
		if err != nil {
			t.Fatalf("unconfigured must not error: %v", err)
		}
		if devs := devicesFromPayload(t, payload); len(devs) != 0 {
			t.Errorf("want empty list, got %+v", devs)
		}
	})
}

// TestMaSelectPlayer pins the POST /ma/player/select contract: known
// player -> {"ok":true} + selection persisted; unknown or unconfigured ->
// ErrBadRequest (HTTP 400).
func TestMaSelectPlayer(t *testing.T) {
	t.Parallel()

	t.Run("known player is selected", func(t *testing.T) {
		srv, _ := newMaFakeServer(t)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))

		payload, err := p.maSelectPlayer(context.Background(), "wiim_uuid:A")
		if err != nil {
			t.Fatalf("maSelectPlayer: %v", err)
		}
		m, ok := payload.(map[string]any)
		if !ok || m["ok"] != true {
			t.Errorf("payload: %+v", payload)
		}
		if p.maActivePlayerId != "wiim_uuid:A" {
			t.Errorf("selection not persisted: %q", p.maActivePlayerId)
		}
	})

	t.Run("unknown player is a bad request", func(t *testing.T) {
		srv, _ := newMaFakeServer(t)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))

		_, err := p.maSelectPlayer(context.Background(), "does-not-exist")
		if !errors.Is(err, ErrBadRequest) {
			t.Errorf("want ErrBadRequest, got %v", err)
		}
	})

	t.Run("unconfigured is a bad request", func(t *testing.T) {
		p := newMaTestPlayer(t, nil)
		_, err := p.maSelectPlayer(context.Background(), "wiim_uuid:A")
		if !errors.Is(err, ErrBadRequest) {
			t.Errorf("want ErrBadRequest, got %v", err)
		}
	})

	t.Run("unreachable server is a generic error", func(t *testing.T) {
		srv, _ := newMaFakeServer(t)
		url := srv.URL
		srv.Close()

		p := newMaTestPlayer(t, maTestSettings(url, "ma"))
		_, err := p.maSelectPlayer(context.Background(), "wiim_uuid:A")
		if err == nil {
			t.Fatal("want error for unreachable MA server")
		}
		if errors.Is(err, ErrBadRequest) {
			t.Errorf("unreachable must not look like a client-side 400: %v", err)
		}
	})
}

// TestMaControlTarget_Errors pins the control-path guards.
func TestMaControlTarget_Errors(t *testing.T) {
	t.Parallel()
	if _, _, err := (newMaTestPlayer(t, nil)).maControlTarget(); !errors.Is(err, ErrBadRequest) {
		t.Errorf("unconfigured: want ErrBadRequest, got %v", err)
	}
	p := newMaTestPlayer(t, maTestSettings("http://h:8095", "ma"))
	if _, _, err := p.maControlTarget(); !errors.Is(err, ErrBadRequest) {
		t.Errorf("no selection: want ErrBadRequest, got %v", err)
	}
}

// TestMaSend_VerbRouting pins the control-verb -> MA command mapping end
// to end (issue #134), including the volume scale math and the ms->s seek
// conversion.
func TestMaSend_VerbRouting(t *testing.T) {
	t.Parallel()

	srv, last := newMaFakeServer(t)
	p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
	p.maActivePlayerId = "wiim_uuid:A"
	ctx := context.Background()

	type caseT struct {
		name    string
		verb    string
		data    any
		wantCmd string
		wantArg map[string]any
	}
	cases := []caseT{
		{"resume maps to play", "resume", nil, "players/cmd/play", nil},
		{"pause", "pause", nil, "players/cmd/pause", nil},
		{"playpause", "playpause", nil, "players/cmd/play_pause", nil},
		{"next", "next", nil, "players/cmd/next", nil},
		{"prev", "prev", nil, "players/cmd/previous", nil},
		{"seek absolute ms->s", "seek", ApiRequestDataSeek{Position: 90000}, "players/cmd/seek", map[string]any{"position": float64(90)}},
		{"volume absolute (52428 == level 80)", "volume", ApiRequestDataVolume{Volume: int32(52428)}, "players/cmd/volume_set", map[string]any{"volume_level": float64(80)}},
		{"shuffle", "shuffle", ApiRequestDataShuffle{Shuffle: true}, "player_queues/shuffle", map[string]any{"queue_id": "wiim_uuid:A", "shuffle_enabled": true}},
		{"repeat context on -> all", "repeat_context", true, "players/cmd/repeat", map[string]any{"repeat_mode": ma.RepeatAll}},
		{"repeat track on -> one", "repeat_track", true, "players/cmd/repeat", map[string]any{"repeat_mode": ma.RepeatOne}},
		{"repeat context off", "repeat_context", false, "players/cmd/repeat", map[string]any{"repeat_mode": ma.RepeatOff}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := p.maSend(ctx, tc.verb, tc.data); err != nil {
				t.Fatalf("maSend(%q): %v", tc.verb, err)
			}
			env := *last
			if env["command"] != tc.wantCmd {
				t.Fatalf("command: got %v want %q", env["command"], tc.wantCmd)
			}
			args, _ := env["args"].(map[string]any)
			if args["player_id"] != "wiim_uuid:A" && env["command"] != "player_queues/shuffle" {
				t.Errorf("player_id: got %v", args["player_id"])
			}
			for k, want := range tc.wantArg {
				if args[k] != want {
					t.Errorf("arg %s: got %v want %v", k, args[k], want)
				}
			}
		})
	}

	t.Run("seek relative is rejected without touching MA", func(t *testing.T) {
		*last = map[string]any{} // clear capture
		err := p.maSend(ctx, "seek", ApiRequestDataSeek{Position: 1000, Relative: true})
		if err == nil {
			t.Fatal("relative seek must be rejected")
		}
		if got := (*last)["command"]; got != nil {
			t.Errorf("no command may be sent for relative seek, got %v", got)
		}
	})

	t.Run("volume relative resolves against last state volume", func(t *testing.T) {
		p.state.remoteState = &RemoteState{Volume: uint32(80 * MaxStateVolume / 100)}
		if err := p.maSend(ctx, "volume", ApiRequestDataVolume{Volume: 10000, Relative: true}); err != nil {
			t.Fatalf("maSend(volume rel): %v", err)
		}
		args, _ := (*last)["args"].(map[string]any)
		want := (52428 + 10000) * 100 / MaxStateVolume // 95
		if args["volume_level"] != float64(want) {
			t.Errorf("relative volume: got %v want level %d", args["volume_level"], want)
		}
	})

	t.Run("unknown verb is an error", func(t *testing.T) {
		if err := p.maSend(ctx, "bogus", nil); err == nil {
			t.Error("unknown verb must fail")
		}
	})
}
