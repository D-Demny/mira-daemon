package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

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

	p.applyMaState(pl, nil, nil)

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

	p.applyMaState(pl, nil, nil)

	rs := p.state.remoteState
	if rs == nil {
		t.Fatal("remoteState must be set")
	}
	if rs.IsPlaying || !rs.IsPaused || rs.PlaybackSpeed != 0 {
		t.Errorf("paused flags: %+v", rs)
	}
}

// TestApplyMaState_QueuePositionWins pins issue #46 (defect A): for some
// providers (live-verified 'sendspin') the PLAYER-TOP position pair is
// stale/zeroed garbage - the QUEUE pair from player_queues/get is canonical
// there: live while playing, frozen at the paused position. Presence of both
// queue fields is the only gate: NO check on queue.State or pl.PlaybackState
// (sendspin reports idle while paused yet must keep its position).
func TestApplyMaState_QueuePositionWins(t *testing.T) {
	t.Parallel()
	p := newMaTestPlayer(t, nil)
	pl := maPlayingPlayer()
	pl.PlaybackState = "paused"
	zero := float64(0)
	pl.ElapsedTime = &zero // sendspin's stale/zeroed player-top (issue #46)
	pl.ElapsedTimeLastUpdated = &zero

	q := &ma.PlayerQueue{
		QueueID:                "queue-1",
		Items:                  3,
		State:                  "idle", // stale provider state - must not gate
		CurrentIndex:           new(int(0)),
		ElapsedTime:            new(float64(21.5)),
		ElapsedTimeLastUpdated: new(float64(1700000123.25)),
	}

	p.applyMaState(pl, q, nil)

	rs := p.state.remoteState
	if rs == nil {
		t.Fatal("remoteState must be set")
	}
	if rs.PositionAsOfTimestamp != 21500 {
		t.Errorf("queue position: got %d want 21500 (queue pair beats player-top)", rs.PositionAsOfTimestamp)
	}
	if rs.Timestamp != 1700000123250 { // epoch seconds -> unix ms
		t.Errorf("queue timestamp: got %d want 1700000123250", rs.Timestamp)
	}
}

// TestApplyMaState_QueueFallbackToPlayerTop pins the issue #46 else-branch:
// a queue response WITHOUT the position pair (the older MA wire shape) keeps
// the existing player-top mapping unchanged.
func TestApplyMaState_QueueFallbackToPlayerTop(t *testing.T) {
	t.Parallel()
	p := newMaTestPlayer(t, nil)
	pl := maPlayingPlayer() // player top: 42.5s @ 1700000000

	p.applyMaState(pl, &ma.PlayerQueue{QueueID: "queue-2", Items: 1, State: "playing"}, nil)

	rs := p.state.remoteState
	if rs.PositionAsOfTimestamp != 42500 || rs.Timestamp != 1700000000000 {
		t.Errorf("player-top fallback: got %d/%d want 42500/1700000000000", rs.PositionAsOfTimestamp, rs.Timestamp)
	}
}

// TestApplyMaIdleState_CurrentMediaPosition pins issue #46 (defect B): a
// provider that pauses by freezing its queue position (sendspin) keeps the
// paused position inside the STALE current_media object of an IDLE poll -
// the synthesized paused state must take it over the last live observation.
// Without the nested pair, the old carry-over of the last track position
// holds (and no new MA fetch is added in this path).
func TestApplyMaIdleState_CurrentMediaPosition(t *testing.T) {
	t.Parallel()

	t.Run("nested current_media pair wins", func(t *testing.T) {
		p := newMaTestPlayer(t, nil)
		pl := maPlayingPlayer()
		pl.PlaybackState = "idle" // media intentionally still set (stale)
		cm := *pl.CurrentMedia
		cm.ElapsedTime = new(float64(77.5))
		cm.ElapsedTimeLastUpdated = new(float64(1700000200.75))
		pl.CurrentMedia = &cm

		p.applyMaState(pl, nil, nil) // idle poll -> applyMaIdleState

		rs := p.state.remoteState
		if rs == nil {
			t.Fatal("idle poll must keep remoteState")
		}
		if !rs.IsPaused || rs.IsPlaying {
			t.Errorf("idle state: %+v", rs)
		}
		if rs.PositionAsOfTimestamp != 77500 {
			t.Errorf("position: got %d want 77500 (from the current_media pair)", rs.PositionAsOfTimestamp)
		}
		if rs.Timestamp != 1700000200750 {
			t.Errorf("timestamp: got %d want 1700000200750", rs.Timestamp)
		}
	})

	t.Run("no nested pair keeps the last observed position", func(t *testing.T) {
		p := newMaTestPlayer(t, nil)
		p.applyMaState(maPlayingPlayer(), nil, nil) // seed: 42.5s live observation
		pl := maPlayingPlayer()
		pl.PlaybackState = "idle"
		p.applyMaState(pl, nil, nil)

		rs := p.state.remoteState
		if rs.PositionAsOfTimestamp != 42500 {
			t.Errorf("carry-over: got %d want 42500 (last live observation)", rs.PositionAsOfTimestamp)
		}
	})
}

// TestApplyMaState_IdleKeepsSelection (issue #138, defects 1+2): live MA
// players keep a non-null (stale) current_media after going idle, so
// PlaybackState gates first. An idle poll of the selected player must NOT
// drop to the device picker: it synthesizes a paused RemoteState carrying
// the last known track, keeping ownership and the device addressable.
func TestApplyMaState_IdleKeepsSelection(t *testing.T) {
	t.Parallel()
	p := newMaTestPlayer(t, nil)
	p.applyMaState(maPlayingPlayer(), nil, nil) // seed a playing state
	if p.state.remoteState == nil {
		t.Fatal("seed failed")
	}

	pl := maPlayingPlayer()
	pl.PlaybackState = "idle" // media intentionally still set (stale)
	p.applyMaState(pl, nil, nil)

	rs := p.state.remoteState
	if rs == nil {
		t.Fatal("idle poll must keep remoteState (device stays selected)")
	}
	if rs.DeviceId != "wiim_uuid:A" || rs.IsPlaying || !rs.IsPaused {
		t.Errorf("idle state: %+v", rs)
	}
	if rs.TrackUri != "spotify--x://track/abc" || rs.TrackName != "Glide" ||
		rs.TrackArtist != "NEIKED/Portugal. The Man" || rs.Duration != 184000 {
		t.Errorf("last track must be shown: %+v", rs)
	}
	if len(rs.NextTracks) != 0 {
		t.Errorf("idle queue has nothing upcoming: %+v", rs.NextTracks)
	}
	if !p.maOwnsRemoteState {
		t.Error("ownership must be kept on idle")
	}
	if p.maLastActiveState == nil || p.maLastActiveState.TrackUri != "spotify--x://track/abc" {
		t.Errorf("live state must be remembered for the idle display, got %+v", p.maLastActiveState)
	}
}

// TestApplyMaState_NullMediaKeepsSelection (issue #138, defect 1): no media
// at all is also an idle signal - and a selection that was never seen live
// (fresh app start) still keeps the device selected with a title-less
// paused card until the poll tick's playlog fetch fills the last song.
func TestApplyMaState_NullMediaKeepsSelection(t *testing.T) {
	t.Parallel()
	p := newMaTestPlayer(t, nil)
	pl := maPlayingPlayer()
	pl.CurrentMedia = nil

	p.applyMaState(pl, nil, nil)

	rs := p.state.remoteState
	if rs == nil {
		t.Fatal("null-media poll must keep remoteState (device stays selected)")
	}
	if rs.DeviceId != "wiim_uuid:A" || !rs.IsPaused || rs.IsPlaying || rs.TrackUri != "" {
		t.Errorf("title-less idle state: %+v", rs)
	}
}

// TestMaPollTick_IdleFetchesPerQueuePlaylog (issue #138, defect 1): an idle
// selection with no live state fetches the speaker's OWN last played track
// once (per-queue playlog filter) and shows it in the synthesized paused
// state; a second tick does not re-fetch.
func TestMaPollTick_IdleFetchesPerQueuePlaylog(t *testing.T) {
	t.Parallel()
	idle := `{
	  "player_id":"wiim_uuid:A","name":"WiiM Kueche","available":true,"playback_state":"idle",
	  "group_members":["wiim_uuid:A"],"synced_to":null,
	  "current_media":{"uri":"stale://track/x","title":"Stale","artist":"","album":"","image_url":"","duration":10}
}`
	recent := `[{"item_id":"t9","provider":"spotify--x","name":"Last Song","uri":"spotify--x://track/last","media_type":"track",
	  "artists":[{"name":"DJ"}],"album":"Mix","duration":200,
	  "image":{"type":"thumb","path":"https://i.scdn.co/image/r1","provider":"spotify--x","remotely_accessible":true}}]`
	srv, envs := newMaRecordingServer(t, map[string]string{
		"players/get":                 idle,
		"music/recently_played_items": recent,
	})
	p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
	p.maActivePlayerId = "wiim_uuid:A"

	p.maPollTick(context.Background())

	rs := p.state.remoteState
	if rs == nil || !rs.IsPaused || rs.TrackUri != "spotify--x://track/last" || rs.TrackName != "Last Song" {
		t.Fatalf("idle state must show the playlog last song: %+v", rs)
	}
	if rs.Duration != 200000 {
		t.Errorf("duration: got %d want 200000", rs.Duration)
	}

	p.maPollTick(context.Background()) // tried-guard: no second fetch
	waitMaWarmupDone(t, p)             // join the tick's background warmup before reading commands

	fetched := 0
	for _, e := range *envs {
		if c, _ := e["command"].(string); c == "music/recently_played_items" {
			fetched++
		}
	}
	if fetched != 1 {
		t.Errorf("per-queue playlog must be fetched exactly once, got %d", fetched)
	}
	if args := envArgs(t, *envs, "music/recently_played_items"); args["queue_id"] != "wiim_uuid:A" {
		t.Errorf("queue_id filter must carry the player's own id: %v", args["queue_id"])
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
			p.applyMaState(maPlayingPlayer(), nil, nil)
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
	if _, _, err := (newMaTestPlayer(t, nil)).maControlTarget(context.Background()); !errors.Is(err, ErrBadRequest) {
		t.Errorf("unconfigured: want ErrBadRequest, got %v", err)
	}
	p := newMaTestPlayer(t, maTestSettings("http://h:8095", "ma"))
	if _, _, err := p.maControlTarget(context.Background()); !errors.Is(err, ErrBadRequest) {
		t.Errorf("no selection: want ErrBadRequest, got %v", err)
	}
}

// ---------- issue #48: MA default device fallback in the control target ----------

// TestMaControlTarget_DefaultDevice pins issue #48: with no player selected
// yet, a control command RETARGETS to the blob's "maDefaultDeviceId" when it
// resolves via players/all (bare id, "group:<master>" or a synced id -> its
// master) - a PLAIN re-target (no queue transfer) that persists the resolved
// id. No default key, a wrong-typed value, an unknown/stale id or a failed
// players/all all keep the legacy ErrBadRequest and persist nothing.
func TestMaControlTarget_DefaultDevice(t *testing.T) {
	t.Parallel()
	routes := map[string]string{"players/all": maTestGroupPlayersJSON}

	// def == "" omits the key entirely (old UI blobs never carry it).
	defaultSettings := func(url, def string) []byte {
		if def == "" {
			return maTestSettings(url, "ma")
		}
		return []byte(fmt.Sprintf(`{"ma":{"url":%q,"token":"tok"},"playbackSource":"ma","maDefaultDeviceId":%q}`, url, def))
	}

	okTarget := func(t *testing.T, p *AppPlayer, want string) {
		t.Helper()
		client, id, err := p.maControlTarget(context.Background())
		if err != nil {
			t.Fatalf("maControlTarget: %v", err)
		}
		if client == nil || id != want {
			t.Fatalf("target: got (%v, %q) want (client, %q)", client, id, want)
		}
		if p.maActivePlayerId != want {
			t.Errorf("retarget must persist the selection, got %q", p.maActivePlayerId)
		}
	}

	badRequest := func(t *testing.T, p *AppPlayer) {
		t.Helper()
		if _, _, err := p.maControlTarget(context.Background()); !errors.Is(err, ErrBadRequest) {
			t.Errorf("want ErrBadRequest, got %v", err)
		}
		if p.maActivePlayerId != "" {
			t.Errorf("failed fallback must not persist a selection, got %q", p.maActivePlayerId)
		}
	}

	t.Run("bare default id resolves and is persisted", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, defaultSettings(srv.URL, "solo-1"))
		okTarget(t, p, "solo-1")
		assertOnlyCommands(t, *envs, []string{"players/all"})
	})

	t.Run("group default resolves to the master", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, defaultSettings(srv.URL, "group:wiim_uuid:A"))
		okTarget(t, p, "wiim_uuid:A")
		assertOnlyCommands(t, *envs, []string{"players/all"})
	})

	t.Run("synced default resolves to its master", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, defaultSettings(srv.URL, "wiim_uuid:B"))
		okTarget(t, p, "wiim_uuid:A")
		assertOnlyCommands(t, *envs, []string{"players/all"})
	})

	t.Run("stale default id keeps the bad request", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, defaultSettings(srv.URL, "does-not-exist"))
		badRequest(t, p)
		assertOnlyCommands(t, *envs, []string{"players/all"})
	})

	t.Run("no default key keeps the bad request", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		badRequest(t, p)
		assertOnlyCommands(t, *envs, nil) // no players/all may be sent at all
	})

	t.Run("wrong-typed default keeps the bad request", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, routes)
		blob := []byte(fmt.Sprintf(`{"ma":{"url":%q,"token":"tok"},"playbackSource":"ma","maDefaultDeviceId":123}`, srv.URL))
		p := newMaTestPlayer(t, blob)
		badRequest(t, p)
		assertOnlyCommands(t, *envs, nil) // the reader rejects non-strings without a lookup
	})

	t.Run("players/all failure keeps the bad request", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, nil) // unrouted -> 500
		p := newMaTestPlayer(t, defaultSettings(srv.URL, "solo-1"))
		badRequest(t, p)
		assertOnlyCommands(t, *envs, []string{"players/all"})
	})
}

// ---------- issue #47: track id extraction for Music Assistant URIs ----------

// TestTrackIdFromTrackURI pins the observer-status track-id rule (issue #47):
// the legacy three-part Spotify Connect form keeps its behavior for ANY
// scheme; Music Assistant media URIs yield the id only for the "track" domain
// - non-track domains (episodes, local files, streams) stay "".
func TestTrackIdFromTrackURI(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"spotify:track:abc", "abc"},         // legacy 3-part form unchanged
		{"spotify:album:def", "def"},         // any scheme: unchanged behavior
		{"spotify:track:abc:def", "abc:def"}, // third part keeps remaining colons
		{"spotify--yofvZfhZ://track/4ml1Eq4wmcdfN0Q6pNEly7", "4ml1Eq4wmcdfN0Q6pNEly7"}, // MA track URI (issue #47)
		{"spotify://track/abc", "abc"},   // MA URI without instance suffix
		{"local://episode/xyz", ""},      // non-track domain: out of scope
		{"spotify--x://stream/live", ""}, // streams: out of scope
		{"spotify--x://track/", ""},      // track domain without id
		{"garbage", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := trackIdFromTrackURI(c.in); got != c.want {
			t.Errorf("trackIdFromTrackURI(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

// TestObserverStatus_MaTrackId pins issue #47 at the request level: an MA-mode
// RemoteState with a provider track URI must yield a NON-EMPTY track_id plus
// the matching lyrics_url - the old 3-part split parsed exactly zero MA URIs,
// leaving lyrics dead in Music Assistant mode.
func TestObserverStatus_MaTrackId(t *testing.T) {
	t.Parallel()
	p := newMaTestPlayer(t, nil)
	p.state.remoteState = &RemoteState{
		DeviceId:   "wiim_uuid:A",
		DeviceName: "WiiM Wohnzimmer",
		DeviceType: maDeviceType,
		TrackUri:   "spotify--yofvZfhZ://track/4ml1Eq4wmcdfN0Q6pNEly7",
		TrackName:  "Glide",
	}

	resp, err := p.handleApiRequest(context.Background(), ApiRequest{Type: ApiRequestTypeObserverStatus})
	if err != nil {
		t.Fatalf("handleApiRequest: %v", err)
	}
	m, ok := resp.(map[string]any)
	if !ok {
		t.Fatalf("observer status must be a map: %T", resp)
	}
	if m["track_id"] != "4ml1Eq4wmcdfN0Q6pNEly7" {
		t.Errorf("track_id: got %v want 4ml1Eq4wmcdfN0Q6pNEly7", m["track_id"])
	}
	if m["lyrics_url"] != "/lyrics/4ml1Eq4wmcdfN0Q6pNEly7" {
		t.Errorf("lyrics_url: got %v want /lyrics/4ml1Eq4wmcdfN0Q6pNEly7", m["lyrics_url"])
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
		// issue #138 round 4, defect A': the player below has no observed state
		// (fresh test player = "" i.e. not playing) - resume/playpause must go
		// through the pause-aware queue command, never the legacy players/cmd
		// play path that restarts a paused track at 0.
		{"resume maps to queue resume (not playing)", "resume", nil, "player_queues/resume", map[string]any{"queue_id": "wiim_uuid:A"}},
		{"pause", "pause", nil, "players/cmd/pause", nil},
		{"playpause (not playing) maps to queue resume", "playpause", nil, "player_queues/resume", map[string]any{"queue_id": "wiim_uuid:A"}},
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
			if args["player_id"] != "wiim_uuid:A" && env["command"] != "player_queues/shuffle" && env["command"] != "player_queues/resume" {
				t.Errorf("player_id: got %v", args["player_id"])
			}
			for k, want := range tc.wantArg {
				if args[k] != want {
					t.Errorf("arg %s: got %v want %v", k, args[k], want)
				}
			}
		})
	}

	// issue #138 round 4, defect A': pause-aware toggle - PAUSED resumes via
	// the queue command (NOT players/cmd/play_pause, which restarts the track
	// at 0), PLAYING pauses via the legacy command, and a resume tap while
	// PLAYING is a no-op.
	t.Run("paused playpause maps to queue resume", func(t *testing.T) {
		p.maLastPlaybackState = "paused"
		if err := p.maSend(ctx, "playpause", nil); err != nil {
			t.Fatalf("maSend(playpause): %v", err)
		}
		env := *last
		if env["command"] != "player_queues/resume" {
			t.Fatalf("command: got %v want player_queues/resume", env["command"])
		}
		args, _ := env["args"].(map[string]any)
		if args["queue_id"] != "wiim_uuid:A" {
			t.Errorf("queue_id: got %v", args["queue_id"])
		}
	})

	t.Run("playing playpause maps to pause", func(t *testing.T) {
		p.maLastPlaybackState = "playing"
		if err := p.maSend(ctx, "playpause", nil); err != nil {
			t.Fatalf("maSend(playpause): %v", err)
		}
		env := *last
		if env["command"] != "players/cmd/pause" {
			t.Fatalf("command: got %v want players/cmd/pause", env["command"])
		}
	})

	t.Run("resume while playing is a no-op", func(t *testing.T) {
		p.maLastPlaybackState = "playing"
		*last = map[string]any{} // clear capture
		if err := p.maSend(ctx, "resume", nil); err != nil {
			t.Fatalf("maSend(resume): %v", err)
		}
		if got := (*last)["command"]; got != nil {
			t.Errorf("no command may be sent for a playing resume, got %v", got)
		}
	})

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

// ---------- issue #138: upcoming-queue mapping, play-uri routing, content endpoints ----------

// TestApplyMaState_Queue pins how the reconstructed upcoming list lands in
// the shared RemoteState plus the carry-over on a transient fetch failure
// (issue #138). The queue response carries NO item list - only source +
// index - so the reconstruction itself lives in maUpcomingTracks and is
// tested separately; here the queue argument only decides pass-through vs
// carry-over.
func TestApplyMaState_Queue(t *testing.T) {
	t.Parallel()

	t.Run("upcoming lands in NextTracks and PrevTracks stays empty", func(t *testing.T) {
		p := newMaTestPlayer(t, nil)
		q := ma.PlayerQueue{
			QueueID:      "wiim_uuid:A",
			Items:        11,
			State:        "playing",
			CurrentIndex: new(int(3)),
			Sources: []ma.QueueSource{{
				ItemID:    "61",
				Provider:  "library",
				Name:      "257ers",
				URI:       "library://playlist/61",
				MediaType: "playlist",
			}},
		}
		upcoming := []QueueTrack{
			{TrackId: "4", Uri: "spotify--x://track/one", Name: "One", Artist: "A, B",
				Album: "The World Is Yours", ImageUrl: "https://cdn.example/one.jpg"},
			{TrackId: "5", Uri: "local://track/song", Name: "Local Song"},
		}
		p.applyMaState(maPlayingPlayer(), &q, upcoming)

		rs := p.state.remoteState
		if len(rs.NextTracks) != 2 {
			t.Fatalf("next tracks: got %d want 2", len(rs.NextTracks))
		}
		n1 := rs.NextTracks[0]
		if n1.TrackId != "4" || n1.Uri != "spotify--x://track/one" || n1.Name != "One" || n1.Artist != "A, B" ||
			n1.Album != "The World Is Yours" || n1.ImageUrl != "https://cdn.example/one.jpg" {
			t.Errorf("next 0: %+v", n1)
		}
		if rs.NextTracks[1].Name != "Local Song" || rs.NextTracks[1].Artist != "" {
			t.Errorf("next 1: %+v", rs.NextTracks[1])
		}
		// MA answers no previous track at all - PrevTracks is empty by design
		if len(rs.PrevTracks) != 0 {
			t.Errorf("prev tracks must be empty, got %+v", rs.PrevTracks)
		}
		// issue #138 defect 3: a single-source queue exposes its URI so the UI
		// can tap upcoming tracks with context+offset instead of raw indices
		if rs.ContextUri != "library://playlist/61" {
			t.Errorf("context uri must expose the single queue source, got %q", rs.ContextUri)
		}
	})

	t.Run("nil queue carries the previous list over", func(t *testing.T) {
		p := newMaTestPlayer(t, nil)
		q := ma.PlayerQueue{State: "playing", Sources: []ma.QueueSource{{URI: "library://playlist/61"}}}
		list := []QueueTrack{{TrackId: "4", Uri: "spotify--x://track/one", Name: "One"}}
		p.applyMaState(maPlayingPlayer(), &q, list)

		// transient player_queues/get failure (queue nil) while media is
		// still active on the same device: keep the pane, don't blank it
		p.applyMaState(maPlayingPlayer(), nil, nil)

		got := p.state.remoteState.NextTracks
		if len(got) != 1 || got[0].TrackId != "4" {
			t.Errorf("carry-over failed: %+v", got)
		}
		if ctx := p.state.remoteState.ContextUri; ctx != "library://playlist/61" {
			t.Errorf("context uri must carry over with the queue, got %q", ctx)
		}
	})

	t.Run("nil queue with a foreign previous device does not leak its list", func(t *testing.T) {
		p := newMaTestPlayer(t, nil)
		prev := &RemoteState{DeviceId: "spotify-dev-1", NextTracks: []QueueTrack{{TrackId: "foreign"}}}
		p.state.remoteState = prev // cluster-owned, different device
		p.maOwnsRemoteState = false

		p.applyMaState(maPlayingPlayer(), nil, nil)

		if got := p.state.remoteState.NextTracks; len(got) != 0 {
			t.Errorf("foreign list must not be carried: %+v", got)
		}
	})
}

// TestMaSend_PlayUriRouting pins the /player/play URI mapping in MA mode
// (issue #138): a context+offset tap resolves AGAINST THE LIVE QUEUE first -
// the tapped uri sitting in it becomes an in-queue play_index skip
// (queue- and autoplay-preserving, defect C); outside the queue the legacy
// play_media(context, start_item) sequence holds with a single-item fallback
// on failure. Bare uri in the observed upcoming list -> play_index, anything
// else -> single-item context, no uri -> resume semantics.
func TestMaSend_PlayUriRouting(t *testing.T) {
	t.Parallel()
	srv, last := newMaFakeServer(t)
	p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
	p.maActivePlayerId = "wiim_uuid:A"
	ctx := context.Background()

	t.Run("offset uri outside the live queue keeps the legacy play_media sequence", func(t *testing.T) {
		// newMaFakeServer answers the membership probe (player_queues/get +
		// player_queues/items) with null -> no match -> legacy path below.
		play := ApiRequestDataPlay{Uri: "library://playlist/61", Offset: &ApiRequestPlayOffset{Position: 3, Uri: "spotify--x://track/xyz"}}
		if err := p.maSend(ctx, "play", play); err != nil {
			t.Fatalf("maSend(play): %v", err)
		}
		env := *last
		if env["command"] != "player_queues/play_media" {
			t.Fatalf("command: got %v want player_queues/play_media", env["command"])
		}
		args, _ := env["args"].(map[string]any)
		if args["queue_id"] != "wiim_uuid:A" || args["option"] != ma.QueueOptionReplace {
			t.Errorf("args: %+v", args)
		}
		media, _ := args["media"].([]any)
		if len(media) != 1 || media[0] != "library://playlist/61" {
			t.Errorf("media: %v", args["media"])
		}
		if args["start_item"] != "spotify--x://track/xyz" {
			t.Errorf("start_item: %v", args["start_item"])
		}
	})

	t.Run("tapped uri inside the live queue skips via play_index", func(t *testing.T) {
		// membership-first (defect C): the tapped uri sits at ABSOLUTE position
		// 3 of the live queue (current_index 4 -> bounded window from offset
		// 2, match at relative index 1) -> exactly ONE control command:
		// play_index with a NUMBER - no play_media at all, so autoplay appends
		// behind the tapped item survive.
		srvM, envs := newMaRecordingServer(t, map[string]string{
			"player_queues/get": `{"queue_id":"wiim_uuid:A","items":6,"current_index":4}`,
			"player_queues/items": `[{"queue_item_id":"qi-2","name":"Before - Song",
			  "media_item":{"uri":"spotify--x://track/before"}},
			 {"queue_item_id":"qi-3","name":"NEIKED - Glide",
			  "media_item":{"item_id":"i-g","name":"Glide","uri":"spotify--x://track/xyz"}}]`,
			"player_queues/play_index": `null`, // tapped uri is in the live queue -> skip via play_index
		})
		pM := newMaTestPlayer(t, maTestSettings(srvM.URL, "ma"))
		pM.maActivePlayerId = "wiim_uuid:A"

		play := ApiRequestDataPlay{Uri: "library://playlist/61", Offset: &ApiRequestPlayOffset{Position: 3, Uri: "spotify--x://track/xyz"}}
		if err := pM.maSend(ctx, "play", play); err != nil {
			t.Fatalf("maSend(play): %v", err)
		}
		assertOnlyCommands(t, *envs, []string{
			"player_queues/get", "player_queues/items", "player_queues/play_index",
		})
		args := envArgs(t, *envs, "player_queues/play_index")
		if args["queue_id"] != "wiim_uuid:A" {
			t.Errorf("queue_id: %+v", args)
		}
		idx, ok := args["index"].(float64) // JSON NUMBER - a string would 400 (defect 3)
		if !ok || int(idx) != 3 {
			t.Errorf("index must be the absolute queue position 3 as a number, got %T %v", args["index"], args["index"])
		}
	})
	t.Run("bare uri matching an observed upcoming track skips in queue", func(t *testing.T) {
		p.state.remoteState = &RemoteState{
			DeviceId:   "wiim_uuid:A",
			NextTracks: []QueueTrack{{TrackId: "4", Uri: "spotify--x://track/abc"}},
		}
		if err := p.maSend(ctx, "play", ApiRequestDataPlay{Uri: "spotify--x://track/abc"}); err != nil {
			t.Fatalf("maSend(play): %v", err)
		}
		env := *last
		if env["command"] != "player_queues/play_index" {
			t.Fatalf("command: got %v want player_queues/play_index", env["command"])
		}
		args, _ := env["args"].(map[string]any)
		if args["queue_id"] != "wiim_uuid:A" {
			t.Errorf("queue_id: %+v", args)
		}
		// MA 2.10.7's play_index schema is int | str: the queue position MUST
		// be a JSON number (a string resolves as queue_item_id -> 400 "Item N
		// not found in queue", issue #138 defect 3)
		idx, ok := args["index"].(float64) // JSON numbers decode to float64
		if !ok || int(idx) != 4 {
			t.Errorf("index must be the JSON number 4, got %T %v", args["index"], args["index"])
		}
	})

	t.Run("non-numeric track id falls through to single-item playback", func(t *testing.T) {
		p.state.remoteState = &RemoteState{
			DeviceId:   "wiim_uuid:A",
			NextTracks: []QueueTrack{{TrackId: "uuid-ish-id", Uri: "spotify--x://track/abc"}},
		}
		if err := p.maSend(ctx, "play", ApiRequestDataPlay{Uri: "spotify--x://track/abc"}); err != nil {
			t.Fatalf("maSend(play): %v", err)
		}
		if got := (*last)["command"]; got != "player_queues/play_media" {
			t.Errorf("non-numeric id must not hit play_index: command %v", got)
		}
	})

	t.Run("offset playback failure falls back to the single tapped item", func(t *testing.T) {
		// dedicated server: the live queue answers WITHOUT the tapped uri
		// (membership probe misses), then the first play_media 500s (stale
		// context) and the retry plays the tapped item alone - mirroring the
		// live re-queue race.
		cmds := []string{}
		var got map[string]any
		srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			var env map[string]any
			_ = json.Unmarshal(b, &env)
			cmd, _ := env["command"].(string)
			args, _ := env["args"].(map[string]any)
			cmds = append(cmds, cmd)
			got = args
			switch {
			case cmd == "player_queues/get":
				_, _ = w.Write([]byte(`{"queue_id":"wiim_uuid:A","items":1,"current_index":0}`))
			case cmd == "player_queues/items":
				_, _ = w.Write([]byte(`[{"queue_item_id":"qi-cur","name":"Current - Song","media_item":{"uri":"spotify--x://track/current"}}]`))
			case cmd == "player_queues/play_media" && len(cmds) == 3:
				http.Error(w, "boom", http.StatusInternalServerError) // first play_media (context + start_item) fails
			default:
				_, _ = w.Write([]byte(`null`))
			}
		}))
		t.Cleanup(srv2.Close)
		p2 := newMaTestPlayer(t, maTestSettings(srv2.URL, "ma"))
		p2.maActivePlayerId = "wiim_uuid:A"

		play := ApiRequestDataPlay{Uri: "library://playlist/61", Offset: &ApiRequestPlayOffset{Position: 3, Uri: "spotify--x://track/xyz"}}
		if err := p2.maSend(ctx, "play", play); err != nil {
			t.Fatalf("maSend(play): %v", err)
		}
		assertCommandSeq(t, cmds, []string{
			"player_queues/get", "player_queues/items", // membership probe: tapped uri absent
			"player_queues/play_media", // legacy context + start_item (500s)
			"player_queues/play_media", // single-item fallback
		})
		if got == nil {
			t.Fatal("no args captured")
		}
		media, _ := got["media"].([]any)
		if len(media) != 1 || media[0] != "spotify--x://track/xyz" {
			t.Errorf("fallback must play the tapped item alone: %+v", got)
		}
		if _, ok := got["start_item"]; ok {
			t.Errorf("fallback must not keep a start_item: %+v", got)
		}
	})

	t.Run("bare uri not in the queue plays as single-item context", func(t *testing.T) {
		p.state.remoteState = &RemoteState{DeviceId: "wiim_uuid:A", NextTracks: []QueueTrack{{TrackId: "qi-7", Uri: "spotify--x://track/other"}}}
		if err := p.maSend(ctx, "play", ApiRequestDataPlay{Uri: "spotify--x://track/solo"}); err != nil {
			t.Fatalf("maSend(play): %v", err)
		}
		env := *last
		if env["command"] != "player_queues/play_media" {
			t.Fatalf("command: got %v want player_queues/play_media", env["command"])
		}
		args, _ := env["args"].(map[string]any)
		if _, ok := args["start_item"]; ok {
			t.Errorf("no start_item expected: %+v", args)
		}
		media, _ := args["media"].([]any)
		if len(media) != 1 || media[0] != "spotify--x://track/solo" {
			t.Errorf("media: %v", args["media"])
		}
	})

	t.Run("queue entries of a different device never match", func(t *testing.T) {
		p.state.remoteState = &RemoteState{
			DeviceId:   "other-player",
			NextTracks: []QueueTrack{{TrackId: "qi-7", Uri: "spotify--x://track/abc"}},
		}
		if err := p.maSend(ctx, "play", ApiRequestDataPlay{Uri: "spotify--x://track/abc"}); err != nil {
			t.Fatalf("maSend(play): %v", err)
		}
		if got := (*last)["command"]; got != "player_queues/play_media" {
			t.Errorf("foreign queue must not be skipped: command %v", got)
		}
	})

	t.Run("play without uri uses the pause-aware queue resume", func(t *testing.T) {
		if err := p.maSend(ctx, "play", ApiRequestDataPlay{}); err != nil {
			t.Fatalf("maSend(play): %v", err)
		}
		env := *last
		// issue #138 round 4, defect A': resume semantics now mean MA's own
		// (pause-aware) queue resume for every not-playing state.
		if env["command"] != "player_queues/resume" {
			t.Fatalf("command: got %v want player_queues/resume", env["command"])
		}
	})
}

// TestMaSend_IdleReplay pins the not-playing-resume semantics (issue #138
// defect A, round 3 A'; round 4 A'): on a NOT-playing MA player (IDLE or
// PAUSED) a resume/play tap asks MA for its OWN queue resume, which
// continues the parked current_item at the saved resume_pos - mid-song after
// a hard stop, NOT from 0; pausing then unpausing forward-resumes at the
// same position instead of restarting. Only when the native resume fails
// (empty queue etc.) does it fall back to replaying the DISPLAYED track: in
// the live queue -> play_index onto it; not in the queue -> play_media with
// the single shown track; everything failed -> plain resume (never a dead
// tap).
func TestMaSend_IdleReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// idleP seeds remoteState with the displayed track (maPlayingPlayer's uri
	// spotify--x://track/abc) as a live observation, then synthesizes the
	// poll's idle state so maIsIdle() is true and the shown track survives in
	// remoteState (maLastActiveState).
	idleP := func(t *testing.T, url string) *AppPlayer {
		t.Helper()
		p := newMaTestPlayer(t, maTestSettings(url, "ma"))
		p.maActivePlayerId = "wiim_uuid:A"
		p.applyMaState(maPlayingPlayer(), nil, nil)
		idle := maPlayingPlayer()
		idle.PlaybackState = "idle"
		p.applyMaIdleState(idle) // records maLastPlaybackState = "idle" + keeps the shown track
		return p
	}

	t.Run("idle resume uses MA's native queue resume", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, map[string]string{"player_queues/resume": `null`})
		p := idleP(t, srv.URL)

		if err := p.maSend(ctx, "resume", nil); err != nil {
			t.Fatalf("maSend(resume): %v", err)
		}
		assertOnlyCommands(t, *envs, []string{"player_queues/resume"}) // no play_index/play_media replay (defect A')
		args := envArgs(t, *envs, "player_queues/resume")
		if qid, _ := args["queue_id"].(string); qid != "wiim_uuid:A" {
			t.Errorf("resume queue_id must be the active player id, got %v", args["queue_id"])
		}
	})

	t.Run("idle resume falls back to replay then plain resume when native resume fails", func(t *testing.T) {
		cmds := []string{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			var env map[string]any
			_ = json.Unmarshal(b, &env)
			cmd, _ := env["command"].(string)
			cmds = append(cmds, cmd)
			switch cmd {
			case "player_queues/resume":
				http.Error(w, "boom", http.StatusInternalServerError) // native resume fails -> fallback chain
			case "player_queues/get":
				_, _ = w.Write([]byte(`{"queue_id":"wiim_uuid:A","items":1,"current_index":0}`))
			case "player_queues/items":
				// no entry carries the displayed uri -> membership probe misses
				_, _ = w.Write([]byte(`[{"queue_item_id":"qi-x","name":"Other - Song","media_item":{"uri":"spotify--x://track/other"}}]`))
			case "player_queues/play_media":
				http.Error(w, "boom", http.StatusInternalServerError) // replay of the shown track fails too
			default:
				_, _ = w.Write([]byte(`null`))
			}
		}))
		t.Cleanup(srv.Close)
		p := idleP(t, srv.URL)

		if err := p.maSend(ctx, "resume", nil); err != nil {
			t.Fatalf("maSend(resume): %v", err)
		}
		assertCommandSeq(t, cmds, []string{
			"player_queues/resume",                     // native resume (500s -> fallback)
			"player_queues/get", "player_queues/items", // membership probe: shown uri absent
			"player_queues/play_media", // single-shown-track replay (500s too)
			"players/cmd/play",         // last resort: plain resume (never a dead tap)
		})
	})

	t.Run("paused resume uses MA's native queue resume", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, map[string]string{"player_queues/resume": `null`})
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		p.maActivePlayerId = "wiim_uuid:A"
		pl := maPlayingPlayer()
		pl.PlaybackState = "paused"
		p.applyMaState(pl, nil, nil) // paused keeps the displayed track + records "paused"

		if err := p.maSend(ctx, "resume", nil); err != nil {
			t.Fatalf("maSend(resume): %v", err)
		}
		// issue #138 round 4, defect A': the legacy players/cmd/play is NOT
		// pause-aware (it restarts a paused track at 0) - unpause goes through
		// player_queues/resume.
		assertOnlyCommands(t, *envs, []string{"player_queues/resume"}) // no play_media restart
	})

	t.Run("idle playpause uses MA's native queue resume", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, map[string]string{"player_queues/resume": `null`})
		p := idleP(t, srv.URL)

		if err := p.maSend(ctx, "playpause", nil); err != nil {
			t.Fatalf("maSend(playpause): %v", err)
		}
		assertOnlyCommands(t, *envs, []string{"player_queues/resume"})
	})

	t.Run("paused playpause uses MA's native queue resume", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, map[string]string{"player_queues/resume": `null`})
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		p.maActivePlayerId = "wiim_uuid:A"
		pl := maPlayingPlayer()
		pl.PlaybackState = "paused"
		p.applyMaState(pl, nil, nil)

		if err := p.maSend(ctx, "playpause", nil); err != nil {
			t.Fatalf("maSend(playpause): %v", err)
		}
		// issue #138 round 4, defect A': players/cmd/play_pause restarts the
		// paused track at 0 (legacy cmd/play fall-through) - unpause goes
		// through player_queues/resume instead.
		assertOnlyCommands(t, *envs, []string{"player_queues/resume"})
	})

	t.Run("idle resume with nothing shown uses the native resume", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, map[string]string{"player_queues/resume": `null`})
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		p.maActivePlayerId = "wiim_uuid:A"
		p.maLastPlaybackState = "idle" // idle with no remoteState (no live observation ever)

		if err := p.maSend(ctx, "resume", nil); err != nil {
			t.Fatalf("maSend(resume): %v", err)
		}
		assertOnlyCommands(t, *envs, []string{"player_queues/resume"}) // nothing to replay behind it
	})

	t.Run("idle resume with nothing shown falls through to plain resume when native resume fails", func(t *testing.T) {
		cmds := []string{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			var env map[string]any
			_ = json.Unmarshal(b, &env)
			cmd, _ := env["command"].(string)
			cmds = append(cmds, cmd)
			if cmd == "player_queues/resume" {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`null`))
		}))
		t.Cleanup(srv.Close)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		p.maActivePlayerId = "wiim_uuid:A"
		p.maLastPlaybackState = "idle" // no remoteState: maResumeDisplayed plain-resumes

		if err := p.maSend(ctx, "resume", nil); err != nil {
			t.Fatalf("maSend(resume): %v", err)
		}
		assertCommandSeq(t, cmds, []string{"player_queues/resume", "players/cmd/play"})
	})
}

// maTestBrowseRoot is the live 2.10.7 root shape: one folder per provider,
// paths ending in "://".
const maTestBrowseRoot = `[
  {"item_id":"root","provider":"builtin","name":"Music Assistant","uri":"builtin://","media_type":"folder","path":"builtin://"},
  {"item_id":"root","provider":"ambient_sounds","name":"Ambient Sounds","uri":"ambient_sounds://","media_type":"folder","path":"ambient_sounds://"},
  {"item_id":"root","provider":"spotify","name":"Spotify","uri":"spotify--yofvZfhZ://","media_type":"folder","path":"spotify--yofvZfhZ://"}
]`

// maTestBrowseBuiltinPlaylists mirrors the live shape: back breadcrumb +
// library playlist items (covers in metadata.images, not top-level).
const maTestBrowseBuiltinPlaylists = `[
  {"item_id":"back","provider":"builtin","name":"..","uri":"builtin://folder/back","media_type":"folder","path":"builtin://"},
  {"item_id":"1","provider":"library","name":"All favorited tracks","uri":"library://playlist/1","media_type":"playlist",
   "metadata":{"images":[{"type":"thumb","path":"/data/playlist_metadata_images/1.jpg","provider":"playlist_metadata","remotely_accessible":false}]}},
  {"item_id":"2","provider":"library","name":"500 Random tracks (from library)","uri":"library://playlist/2","media_type":"playlist",
   "metadata":{"images":[{"type":"thumb","path":"https://i.scdn.co/image/abc","provider":"spotify--yofvZfhZ","remotely_accessible":true}]}}
]`

// maTestBrowseSpotifyPlaylists mirrors the live shape of a synced provider.
const maTestBrowseSpotifyPlaylists = `[
  {"item_id":"back","provider":"spotify--yofvZfhZ","name":"..","uri":"spotify--yofvZfhZ://folder/back","media_type":"folder","path":"spotify--yofvZfhZ://"},
  {"item_id":"61","provider":"library","name":"257ers","uri":"library://playlist/61","media_type":"playlist",
   "metadata":{"images":[{"type":"thumb","path":"https://i.scdn.co/image/xyz","provider":"spotify--yofvZfhZ","remotely_accessible":true}]}}
]`

// maTestPlaylistTracks mirrors the live playlist_tracks shape (library
// provider, absolute position, artists list).
const maTestPlaylistTracks = `[
  {"item_id":"t1","provider":"spotify--yofvZfhZ","name":"Glide","uri":"spotify--x://track/one","media_type":"track","album":"The World Is Yours",
   "artists":[{"name":"NEIKED"},{"name":"Portugal. The Man"}],"duration":184,"position":0,
   "image":{"type":"thumb","path":"https://i.scdn.co/image/a","provider":"spotify--yofvZfhZ","remotely_accessible":true}},
  {"item_id":"t2","provider":"local","name":"Local Jam","uri":"local://track/jam","media_type":"track",
   "album":{"item_id":"al1","provider":"local","name":"Jam Album","uri":"local://album/la"},"position":1,
   "image":{"type":"thumb","path":"/data/local/jam.jpg","provider":"local","remotely_accessible":false}},
  {"item_id":"t3","provider":"spotify--x","name":"","media_type":"track"}
]`

// maTestRecentItems mirrors the play-log mapping: lightweight entries, a
// non-track/non-playlist row that must be filtered out by the client, and -
// issue #138 round 4, defect B' - a playlist row in the exact live shape
// (library item, numeric id, auto-generated uri).
const maTestRecentItems = `[
  {"item_id":"t9","provider":"spotify--x","name":"Recently One","uri":"spotify--x://track/r1","media_type":"track",
   "image":{"type":"thumb","path":"https://i.scdn.co/image/r1","provider":"spotify--yofvZfhZ","remotely_accessible":true}},
  {"item_id":"a1","provider":"spotify--x","name":"Some Album","uri":"spotify--x://album/album","media_type":"album"},
  {"item_id":"t8","provider":"local","name":"Recently Two","uri":"","media_type":"track"},
  {"item_id":"26","provider":"library","name":"DnB","uri":"library://playlist/26","media_type":"playlist"}
]`

// newMaContentFakeServer serves canned per-command payloads (browse keyed by
// "music/browse@<path>"); an unrouted command answers 500 - the same answer
// a provider folder without a playlists collection gives live.
func newMaContentFakeServer(t *testing.T, routes map[string]string) (*httptest.Server, *map[string]any) {
	t.Helper()
	var last map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var env map[string]any
		_ = json.Unmarshal(b, &env)
		last = env
		key, _ := env["command"].(string)
		if key == "music/browse" {
			args, _ := env["args"].(map[string]any)
			path, _ := args["path"].(string)
			key = "music/browse@" + path
		}
		if body, ok := routes[key]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv, &last
}

func playlistsFromPayload(t *testing.T, payload any) []MaPlaylist {
	t.Helper()
	m, ok := payload.(map[string]any)
	if !ok {
		t.Fatalf("payload must be an object: %T", payload)
	}
	data, _ := json.Marshal(m["playlists"])
	var typed []MaPlaylist
	if err := json.Unmarshal(data, &typed); err != nil {
		t.Fatalf("playlists field: %v", err)
	}
	return typed
}

func tracksFromPayload(t *testing.T, payload any) []MaTrack {
	t.Helper()
	m, ok := payload.(map[string]any)
	if !ok {
		t.Fatalf("payload must be an object: %T", payload)
	}
	data, _ := json.Marshal(m["tracks"])
	var typed []MaTrack
	if err := json.Unmarshal(data, &typed); err != nil {
		t.Fatalf("tracks field: %v", err)
	}
	return typed
}

// TestMaListPlaylists pins the GET /ma/playlists contract (issue #138):
// flattened library playlists across provider folders, per-folder failures
// skipped; unconfigured yields an empty list, a configured-but-failing browse errors (-> HTTP 500) so the UI retries instead of caching an empty list (defect 4).
func TestMaListPlaylists(t *testing.T) {
	t.Parallel()

	routes := map[string]string{
		"music/browse@root":                          maTestBrowseRoot,
		"music/browse@builtin://playlists":           maTestBrowseBuiltinPlaylists,
		"music/browse@spotify--yofvZfhZ://playlists": maTestBrowseSpotifyPlaylists,
	}

	t.Run("flattens provider folders into one list", func(t *testing.T) {
		srv, _ := newMaContentFakeServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))

		payload, err := p.maListPlaylists(context.Background())
		if err != nil {
			t.Fatalf("maListPlaylists: %v", err)
		}
		pl := playlistsFromPayload(t, payload)
		if len(pl) != 3 {
			t.Fatalf("want 3 playlists (back-breadcrumbs, non-playlists and the 500-ing provider skipped), got %d: %+v", len(pl), pl)
		}
		want := []struct{ id, name, uri, img string }{
			{"1", "All favorited tracks", "library://playlist/1", ""}, // local-only art -> no url
			{"2", "500 Random tracks (from library)", "library://playlist/2", "https://i.scdn.co/image/abc"},
			{"61", "257ers", "library://playlist/61", "https://i.scdn.co/image/xyz"},
		}
		for i, w := range want {
			if pl[i].ItemID != w.id || pl[i].Name != w.name || pl[i].URI != w.uri || pl[i].ImageURL != w.img {
				t.Errorf("playlist %d: got %+v want %+v", i, pl[i], w)
			}
		}
	})

	t.Run("unconfigured yields empty list", func(t *testing.T) {
		p := newMaTestPlayer(t, nil)
		payload, err := p.maListPlaylists(context.Background())
		if err != nil {
			t.Fatalf("unconfigured must not error: %v", err)
		}
		if pl := playlistsFromPayload(t, payload); len(pl) != 0 {
			t.Errorf("want empty list, got %+v", pl)
		}
	})

	t.Run("configured but unreachable errors", func(t *testing.T) {
		srv, _ := newMaContentFakeServer(t, routes)
		url := srv.URL
		srv.Close()

		p := newMaTestPlayer(t, maTestSettings(url, "ma"))
		if _, err := p.maListPlaylists(context.Background()); err == nil {
			t.Fatal("configured + failed root browse must error (issue #138 defect 4)")
		}
	})

	t.Run("all provider folders failing errors", func(t *testing.T) {
		srv, _ := newMaContentFakeServer(t, map[string]string{
			"music/browse@root": maTestBrowseRoot, // no <provider>://playlists route -> every folder 500s
		})
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		if _, err := p.maListPlaylists(context.Background()); err == nil {
			t.Fatal("all-folder failure must error, not yield an empty list")
		}
	})

	t.Run("failed fetch serves the last good list (stale-while-error)", func(t *testing.T) {
		routes := map[string]string{
			"music/browse@root":                          maTestBrowseRoot,
			"music/browse@builtin://playlists":           maTestBrowseBuiltinPlaylists,
			"music/browse@spotify--yofvZfhZ://playlists": maTestBrowseSpotifyPlaylists,
		}
		srv, _ := newMaContentFakeServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))

		fresh, err := p.maListPlaylists(context.Background())
		if err != nil {
			t.Fatalf("fresh fetch: %v", err)
		}
		if pl := playlistsFromPayload(t, fresh); len(pl) != 3 {
			t.Fatalf("fresh list: %+v", pl)
		}

		// break every route (the fake reads the map per request): a browse
		// failing AFTER one good fetch must serve the stale list, not an
		// error card (defect B).
		for k := range routes {
			delete(routes, k)
		}
		stale, err := p.maListPlaylists(context.Background())
		if err != nil {
			t.Fatalf("stale-while-error must not error: %v", err)
		}
		pl := playlistsFromPayload(t, stale)
		if len(pl) != 3 || pl[0].ItemID != "1" || pl[2].URI != "library://playlist/61" {
			t.Errorf("stale list must be the last good one: %+v", pl)
		}
	})

	t.Run("failed browse with no cached list errors (never an authoritative empty)", func(t *testing.T) {
		srv, _ := newMaContentFakeServer(t, nil) // even root 500s
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		if _, err := p.maListPlaylists(context.Background()); err == nil {
			t.Fatal("no cache + failed browse must error, not yield an empty list (issue #138 defect 4)")
		}
	})
}

// TestMaListPlaylists_RecentOrder pins issue #138 round 4, defect S: the
// /ma/playlists response is served in last-played recency (recently played
// playlists first, newest order), untouched ones keep their browse order,
// and a playlog failure degrades to browse order instead of breaking the
// list.
func TestMaListPlaylists_RecentOrder(t *testing.T) {
	t.Parallel()

	const browse = `[
  {"item_id":"70","provider":"library","name":"A-list","uri":"library://playlist/70","media_type":"playlist"},
  {"item_id":"26","provider":"library","name":"B-dnb","uri":"library://playlist/26","media_type":"playlist"},
  {"item_id":"11","provider":"library","name":"C-jazz","uri":"library://playlist/11","media_type":"playlist"}
]`

	t.Run("recently played playlists come first in recency order", func(t *testing.T) {
		routes := map[string]string{
			"music/browse@root":                          maTestBrowseRoot,
			"music/browse@builtin://playlists":           "[]",
			"music/browse@spotify--yofvZfhZ://playlists": browse,
			// the play log is timestamp DESC: 11 newest, then 70; 26 never
			// played.
			"music/recently_played_items": `[
  {"item_id":"11","provider":"library","name":"C-jazz","uri":"library://playlist/11","media_type":"playlist"},
  {"item_id":"70","provider":"library","name":"A-list","uri":"library://playlist/70","media_type":"playlist"}
]`,
		}
		srv, last := newMaContentFakeServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))

		payload, err := p.maListPlaylists(context.Background())
		if err != nil {
			t.Fatalf("maListPlaylists: %v", err)
		}
		pl := playlistsFromPayload(t, payload)
		wantIDs := []string{"11", "70", "26"}
		if len(pl) != len(wantIDs) {
			t.Fatalf("want order %v, got %+v", wantIDs, pl)
		}
		for i, id := range wantIDs {
			if pl[i].ItemID != id {
				t.Errorf("playlist[%d]: got %s want %s", i, pl[i].ItemID, id)
			}
		}
		args, _ := (*last)["args"].(map[string]any)
		if args["fully_played_only"] != false {
			t.Errorf("ordering playlog must include partial plays: fully_played_only=%v", args["fully_played_only"])
		}
	})

	t.Run("playlog failure keeps the browse order", func(t *testing.T) {
		routes := map[string]string{
			"music/browse@root":                          maTestBrowseRoot,
			"music/browse@builtin://playlists":           "[]",
			"music/browse@spotify--yofvZfhZ://playlists": browse,
			// recently_played_items unrouted -> 500
		}
		srv, _ := newMaContentFakeServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))

		payload, err := p.maListPlaylists(context.Background())
		if err != nil {
			t.Fatalf("a playlog failure must not break the list: %v", err)
		}
		pl := playlistsFromPayload(t, payload)
		wantIDs := []string{"70", "26", "11"} // browse order unchanged
		if len(pl) != len(wantIDs) {
			t.Fatalf("got %+v", pl)
		}
		for i, id := range wantIDs {
			if pl[i].ItemID != id {
				t.Errorf("playlist[%d]: got %s want %s (browse order)", i, pl[i].ItemID, id)
			}
		}
	})
}

// TestMaListPlaylistItems pins the GET /ma/playlist-items contract (issue
// #138): playlist-ordered track cards with absolute position; entries
// without a uri are skipped; empty list on failure.
func TestMaListPlaylistItems(t *testing.T) {
	t.Parallel()

	t.Run("maps playlist tracks in order", func(t *testing.T) {
		srv, _ := newMaContentFakeServer(t, map[string]string{
			"music/playlists/playlist_tracks": maTestPlaylistTracks,
		})
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))

		payload, err := p.maListPlaylistItems(context.Background(), "61")
		if err != nil {
			t.Fatalf("maListPlaylistItems: %v", err)
		}
		tracks := tracksFromPayload(t, payload)
		if len(tracks) != 2 { // the uri-less entry is skipped
			t.Fatalf("want 2 tracks, got %d: %+v", len(tracks), tracks)
		}
		t0 := tracks[0]
		if t0.ID != "t1" || t0.Name != "Glide" || t0.URI != "spotify--x://track/one" ||
			t0.Album != "The World Is Yours" || t0.Position != 0 || t0.DurationMs != 184000 {
			t.Errorf("track 0: %+v", t0)
		}
		if len(t0.Artists) != 2 || t0.Artists[0] != "NEIKED" || t0.Artists[1] != "Portugal. The Man" {
			t.Errorf("track 0 artists: %v", t0.Artists)
		}
		if t0.ImageURL != "https://i.scdn.co/image/a" {
			t.Errorf("track 0 image: %q", t0.ImageURL)
		}
		if tracks[1].Name != "Local Jam" || tracks[1].Position != 1 || tracks[1].ImageURL != "" || // local art not served
			tracks[1].Album != "Jam Album" { // album arrives as an OBJECT on live wire - must decode to its name
			t.Errorf("track 1: %+v", tracks[1])
		}
	})

	t.Run("lookup failure yields empty list", func(t *testing.T) {
		srv, _ := newMaContentFakeServer(t, nil) // unrouted -> 500
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))

		payload, err := p.maListPlaylistItems(context.Background(), "61")
		if err != nil {
			t.Fatalf("failure must not error: %v", err)
		}
		if tracks := tracksFromPayload(t, payload); len(tracks) != 0 {
			t.Errorf("want empty list, got %+v", tracks)
		}
	})

	t.Run("empty id or unconfigured yields empty list", func(t *testing.T) {
		p := newMaTestPlayer(t, nil)
		payload, err := p.maListPlaylistItems(context.Background(), "")
		if err != nil {
			t.Fatalf("must not error: %v", err)
		}
		if tracks := tracksFromPayload(t, payload); len(tracks) != 0 {
			t.Errorf("want empty list, got %+v", tracks)
		}
	})
}

// TestMaListRecent pins the GET /ma/recent contract (issue #138): play-log
// track rows only (album rows and uri-less rows filtered in the client),
// a configured-but-failing playlog fetch errors (defect 4), limit clamping.
func TestMaListRecent(t *testing.T) {
	t.Parallel()

	t.Run("keeps track and playlist rows, pins partial-log args", func(t *testing.T) {
		srv, last := newMaContentFakeServer(t, map[string]string{
			"music/recently_played_items": maTestRecentItems,
		})
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))

		payload, err := p.maListRecent(context.Background(), 20)
		if err != nil {
			t.Fatalf("maListRecent: %v", err)
		}
		args, _ := (*last)["args"].(map[string]any)
		if args["limit"] != float64(20) {
			t.Errorf("limit arg: %v", args["limit"])
		}
		// issue #138 round 4, defect B': partially played items must surface -
		// fully_played_only is sent explicitly as false (the MA API default
		// true is what kept skipped tracks out of "Zuletzt"); playlist rows are
		// requested like in MA's own "Zuletzt gespielt" view.
		if args["fully_played_only"] != false {
			t.Errorf("fully_played_only: got %v want false", args["fully_played_only"])
		}
		wantTypes := []string{"track", "playlist"}
		gotTypes, _ := args["media_types"].([]any)
		if len(gotTypes) != len(wantTypes) {
			t.Fatalf("media_types: got %v want %v", args["media_types"], wantTypes)
		}
		for i, w := range wantTypes {
			if gotTypes[i] != w {
				t.Errorf("media_types[%d]: got %v want %q", i, gotTypes[i], w)
			}
		}
		tracks := tracksFromPayload(t, payload)
		if len(tracks) != 2 {
			t.Fatalf("want 2 rows (album row + uri-less row filtered), got %d: %+v", len(tracks), tracks)
		}
		if tracks[0].Name != "Recently One" || tracks[0].URI != "spotify--x://track/r1" ||
			tracks[0].ImageURL != "https://i.scdn.co/image/r1" || tracks[0].MediaType != "" {
			t.Errorf("track 0: %+v", tracks[0])
		}
		if tracks[1].Name != "DnB" || tracks[1].URI != "library://playlist/26" || tracks[1].MediaType != "playlist" {
			t.Errorf("playlist row 1: %+v", tracks[1])
		}
	})

	t.Run("configured but failing fetch errors", func(t *testing.T) {
		srv, _ := newMaContentFakeServer(t, nil) // unrouted -> 500
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		if _, err := p.maListRecent(context.Background(), 20); err == nil {
			t.Fatal("configured + failed playlog fetch must error (issue #138 defect 4)")
		}
	})

	t.Run("unconfigured yields empty list", func(t *testing.T) {
		p := newMaTestPlayer(t, nil)
		payload, err := p.maListRecent(context.Background(), 20)
		if err != nil {
			t.Fatalf("must not error: %v", err)
		}
		if tracks := tracksFromPayload(t, payload); len(tracks) != 0 {
			t.Errorf("want empty list, got %+v", tracks)
		}
	})

	t.Run("out-of-range limit is clamped to the default", func(t *testing.T) {
		srv, last := newMaContentFakeServer(t, map[string]string{
			"music/recently_played_items": "[]",
		})
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))

		if _, err := p.maListRecent(context.Background(), 0); err != nil {
			t.Fatalf("maListRecent: %v", err)
		}
		args, _ := (*last)["args"].(map[string]any)
		if args["limit"] != float64(20) {
			t.Errorf("clamped limit: got %v want 20", args["limit"])
		}
	})
}

// ---------- issue #138 part 2: upcoming reconstruction, group devices, transfer ----------

// maTestGroupPlayersJSON mirrors the live 2.10.7 grouping fields: the master
// carries every member (itself included) with synced_to null; each slave has
// an empty member list and points at its master via synced_to; a solo player
// names only itself.
const maTestGroupPlayersJSON = `[
  {"player_id":"wiim_uuid:A","name":"WiiM Wohnzimmer","available":true,"playback_state":"playing",
   "group_members":["wiim_uuid:A","wiim_uuid:B"],"synced_to":null},
  {"player_id":"wiim_uuid:B","name":"WiiM Kueche","available":true,"playback_state":"paused",
   "group_members":[],"synced_to":"wiim_uuid:A"},
  {"player_id":"solo-1","name":"Solo","available":true,"playback_state":"idle",
   "group_members":["solo-1"],"synced_to":null}
]`

// TestMaListDevices_GroupEntry pins the synthetic native-group device
// (issue #138): one extra entry per MASTER with more than one member, named
// "<master> (Gruppe)", id "group:<master>"; solo players get none.
func TestMaListDevices_GroupEntry(t *testing.T) {
	t.Parallel()
	srv, _ := newMaContentFakeServer(t, map[string]string{"players/all": maTestGroupPlayersJSON})
	p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))

	payload, err := p.maListDevices(context.Background())
	if err != nil {
		t.Fatalf("maListDevices: %v", err)
	}
	devs := devicesFromPayload(t, payload)
	if len(devs) != 4 { // 3 players + 1 group entry
		t.Fatalf("want 4 devices, got %d: %+v", len(devs), devs)
	}
	g := devs[3]
	if g.Id != "group:wiim_uuid:A" || g.Name != "WiiM Wohnzimmer (Gruppe)" || !g.Available {
		t.Errorf("group entry: %+v", g)
	}
}

// TestMaSelectPlayer_Resolution pins the selection canonicalization
// (issue #138): the synthetic group id and a firmware-synced slave both
// resolve to the group MASTER; unknown ids stay 400. Without an existing
// selection nothing is transferred - only players/all may be sent.
func TestMaSelectPlayer_Resolution(t *testing.T) {
	t.Parallel()
	routes := map[string]string{"players/all": maTestGroupPlayersJSON}

	t.Run("group id resolves to the master", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))

		payload, err := p.maSelectPlayer(context.Background(), "group:wiim_uuid:A")
		if err != nil {
			t.Fatalf("maSelectPlayer: %v", err)
		}
		if m, ok := payload.(map[string]any); !ok || m["ok"] != true {
			t.Errorf("payload: %+v", payload)
		}
		if p.maActivePlayerId != "wiim_uuid:A" {
			t.Errorf("selection must resolve to the master, got %q", p.maActivePlayerId)
		}
		assertOnlyCommands(t, *envs, []string{"players/all"})
	})

	t.Run("synced slave resolves to the master", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))

		if _, err := p.maSelectPlayer(context.Background(), "wiim_uuid:B"); err != nil {
			t.Fatalf("maSelectPlayer: %v", err)
		}
		if p.maActivePlayerId != "wiim_uuid:A" {
			t.Errorf("selection must resolve to the master, got %q", p.maActivePlayerId)
		}
		assertOnlyCommands(t, *envs, []string{"players/all"})
	})

	t.Run("unknown id is a bad request", func(t *testing.T) {
		srv, _ := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		if _, err := p.maSelectPlayer(context.Background(), "group:does-not-exist"); !errors.Is(err, ErrBadRequest) {
			t.Errorf("want ErrBadRequest, got %v", err)
		}
	})
}

// TestMaSelectPlayer_ResetsLastSongSources (issue #138, defect 1): the
// last-song sources are per speaker - a CHANGED target re-derives them (fresh
// per-queue playlog on the first idle poll), re-selecting the same target
// keeps them.
func TestMaSelectPlayer_ResetsLastSongSources(t *testing.T) {
	t.Parallel()
	routes := map[string]string{"players/all": maTestGroupPlayersJSON}

	newP := func(t *testing.T, url string) *AppPlayer {
		p := newMaTestPlayer(t, maTestSettings(url, "ma"))
		p.maActivePlayerId = "wiim_uuid:A"
		p.maLastActiveState = &RemoteState{DeviceId: "wiim_uuid:A", TrackUri: "spotify--x://track/old"}
		p.maIdleLogFallback = &RemoteState{DeviceId: "wiim_uuid:A"}
		p.maIdleLogTriedFor = "wiim_uuid:A"
		return p
	}

	t.Run("changed target clears the old speaker's sources", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, routes)
		p := newP(t, srv.URL)

		if _, err := p.maSelectPlayer(context.Background(), "solo-1"); err != nil {
			t.Fatalf("maSelectPlayer: %v", err)
		}
		if p.maActivePlayerId != "solo-1" {
			t.Errorf("target: got %q want solo-1", p.maActivePlayerId)
		}
		if p.maLastActiveState != nil || p.maIdleLogFallback != nil || p.maIdleLogTriedFor != "" {
			t.Errorf("changed target must reset last-song sources: %+v / %+v / %q", p.maLastActiveState, p.maIdleLogFallback, p.maIdleLogTriedFor)
		}
		assertOnlyCommands(t, *envs, []string{"players/all", "player_queues/get"}) // queue get 500s -> treated as idle -> plain re-target
	})

	t.Run("same target keeps the sources", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, routes)
		p := newP(t, srv.URL)

		if _, err := p.maSelectPlayer(context.Background(), "solo-1"); err != nil { // different target first
			t.Fatalf("maSelectPlayer: %v", err)
		}
		p.maLastActiveState = &RemoteState{DeviceId: "solo-1", TrackUri: "spotify--x://track/new"}

		if _, err := p.maSelectPlayer(context.Background(), "solo-1"); err != nil {
			t.Fatalf("maSelectPlayer (again): %v", err)
		}
		if p.maLastActiveState == nil || p.maLastActiveState.TrackUri != "spotify--x://track/new" {
			t.Errorf("re-selecting the same target must keep the sources, got %+v", p.maLastActiveState)
		}
		assertOnlyCommands(t, *envs, []string{"players/all", "player_queues/get", "players/all"})
	})
}

// maTestQueuePlayingPlaylist is the live 2.10.7 player_queues/get shape: no
// item list - only count, index, state, current/next item and sources.
const maTestQueuePlayingPlaylist = `{
  "queue_id":"wiim_uuid:A","items":3,"current_index":0,"state":"playing",
  "current_item":{"queue_item_id":"qi-1","name":"NEIKED - Glide",
    "media_item":{"item_id":"item-1","name":"Glide","uri":"spotify--x://track/abc","album":"The World Is Yours"}},
  "sources":[{"item_id":"61","provider":"library","name":"257ers",
    "uri":"library://playlist/61","media_type":"playlist"}]
}`

// maTestQueuePlayingSingle is a single-item queue: no sources, the current
// track URI is all there is to replay.
const maTestQueuePlayingSingle = `{
  "queue_id":"wiim_uuid:A","items":1,"current_index":0,"state":"playing",
  "current_item":{"queue_item_id":"qi-9","name":"NEIKED - Glide",
    "media_item":{"item_id":"item-9","name":"Glide","uri":"spotify--x://track/abc"}}
}`

// TestMaTransferPlayback pins the user-approved transfer semantics (issue
// #138, round 3 W): selecting a DIFFERENT target while the old one plays
// moves the music with MA's OWN player_queues/transfer - full queue +
// position copied to the target, source stopped and cleared (MA ungroups a
// grouped/synced target first). Nothing playing = plain re-target (no
// destructive transfer); a failed transfer aborts the selection and keeps
// the old target.
func TestMaTransferPlayback(t *testing.T) {
	t.Parallel()

	t.Run("playing playlist queue transfers to the new target", func(t *testing.T) {
		routes := map[string]string{"players/all": maTestGroupPlayersJSON, "player_queues/get": maTestQueuePlayingPlaylist, "player_queues/transfer": `null`}
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		p.maActivePlayerId = "wiim_uuid:A"

		if _, err := p.maSelectPlayer(context.Background(), "solo-1"); err != nil {
			t.Fatalf("maSelectPlayer: %v", err)
		}
		if p.maActivePlayerId != "solo-1" {
			t.Errorf("selection: %q", p.maActivePlayerId)
		}
		// native transfer: NO play_media, NO pause (MA stops + clears the source itself)
		assertOnlyCommands(t, *envs, []string{"players/all", "player_queues/get", "player_queues/transfer"})
		tr := envArgs(t, *envs, "player_queues/transfer")
		if tr["source_queue_id"] != "wiim_uuid:A" || tr["target_queue_id"] != "solo-1" {
			t.Errorf("transfer args: %+v", tr)
		}
	})

	t.Run("single-item queue transfers as well", func(t *testing.T) {
		routes := map[string]string{"players/all": maTestGroupPlayersJSON, "player_queues/get": maTestQueuePlayingSingle, "player_queues/transfer": `null`}
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		p.maActivePlayerId = "wiim_uuid:A"

		if _, err := p.maSelectPlayer(context.Background(), "solo-1"); err != nil {
			t.Fatalf("maSelectPlayer: %v", err)
		}
		assertOnlyCommands(t, *envs, []string{"players/all", "player_queues/get", "player_queues/transfer"})
		tr := envArgs(t, *envs, "player_queues/transfer")
		if tr["source_queue_id"] != "wiim_uuid:A" || tr["target_queue_id"] != "solo-1" {
			t.Errorf("transfer args: %+v", tr)
		}
	})

	t.Run("idle queue is a plain re-target", func(t *testing.T) {
		routes := map[string]string{
			"players/all":       maTestGroupPlayersJSON,
			"player_queues/get": `{"queue_id":"wiim_uuid:A","items":0,"state":"idle"}`,
		}
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		p.maActivePlayerId = "wiim_uuid:A"

		if _, err := p.maSelectPlayer(context.Background(), "solo-1"); err != nil {
			t.Fatalf("maSelectPlayer: %v", err)
		}
		if p.maActivePlayerId != "solo-1" {
			t.Errorf("selection: %q", p.maActivePlayerId)
		}
		assertOnlyCommands(t, *envs, []string{"players/all", "player_queues/get"}) // no play_media, no pause
	})

	t.Run("re-selecting the same resolved target does not transfer", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, map[string]string{"players/all": maTestGroupPlayersJSON})
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		p.maActivePlayerId = "wiim_uuid:A" // master of the group

		// tapping the slave (or its group entry) resolves to the SAME master:
		// the music must keep playing untouched
		if _, err := p.maSelectPlayer(context.Background(), "wiim_uuid:B"); err != nil {
			t.Fatalf("maSelectPlayer: %v", err)
		}
		assertOnlyCommands(t, *envs, []string{"players/all"})
	})

	t.Run("failed transfer aborts the selection and keeps the old target", func(t *testing.T) {
		routes := map[string]string{"players/all": maTestGroupPlayersJSON, "player_queues/get": maTestQueuePlayingPlaylist}
		srv, _ := newMaRecordingServer(t, routes) // player_queues/transfer unrouted -> 500
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		p.maActivePlayerId = "wiim_uuid:A"

		_, err := p.maSelectPlayer(context.Background(), "solo-1")
		if err == nil {
			t.Fatal("failed transfer must error")
		}
		if errors.Is(err, ErrBadRequest) {
			t.Errorf("a server failure must not look like a client-side 400: %v", err)
		}
		if p.maActivePlayerId != "wiim_uuid:A" {
			t.Errorf("old selection must be kept, got %q", p.maActivePlayerId)
		}
	})
}

// maBlobSettings is a SettingsHandler stub serving one fixed settings blob.
type maBlobSettings struct{ blob []byte }

func (s *maBlobSettings) GetSettings() []byte           { return s.blob }
func (s *maBlobSettings) PutSettings(body []byte) error { return nil }

// TestMaPreSessionContentServe pins issue #138 round 3 (defect B'): while no
// player session exists yet - playerReady=false, the Spotify device-flow
// pairing window after a flash - the /ma/* content endpoints must be served
// directly from Music Assistant off the persisted settings blob (which
// survives the flash), not 503. The response shapes are the exact post-
// session contracts; a non-MA playback source or an unconfigured/failed MA
// keeps the 503-retry contract so the UI never caches an empty list.
func TestMaPreSessionContentServe(t *testing.T) {
	t.Parallel()

	// fake MA with the live 2.10.7 shapes: players, root browse (three
	// providers - only builtin carries a routed playlists collection, the
	// other two 500 and must be skipped), playlist tracks, play log
	masrv, envs := newMaRecordingServer(t, map[string]string{
		"players/all":                      maTestGroupPlayersJSON,
		"music/browse@root":                maTestBrowseRoot,
		"music/browse@builtin://playlists": maTestBrowseBuiltinPlaylists,
		"music/playlists/playlist_tracks":  `[{"item_id":"t-1","provider":"library","name":"Glide","uri":"spotify--x://track/abc","media_type":"track","position":3,"duration":214,"album":{"item_id":"al","name":"The World Is Yours"},"artists":[{"name":"NEIKED"}]}]`,
		"music/recently_played_items":      `[{"item_id":"t-9","provider":"library","name":"Take It All","uri":"spotify--x://track/t9","media_type":"track"}]`,
	})

	stub := &maBlobSettings{blob: maTestSettings(masrv.URL, "ma")}
	srv, base := newTestApiServer(t)
	srv.SetSettingsHandler(stub) // playerReady stays false: pre-session path

	getJSON := func(path string, out any) int {
		t.Helper()
		resp, err := testClient.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("GET %s: decoding body (%d bytes): %v", path, len(b), err)
		}
		return resp.StatusCode
	}

	t.Run("playlists are served pre-session", func(t *testing.T) {
		var got struct {
			Playlists []MaPlaylist `json:"playlists"`
		}
		if code := getJSON("/ma/playlists", &got); code != 200 {
			t.Fatalf("GET /ma/playlists: status %d, want 200 pre-session", code)
		}
		if len(got.Playlists) != 2 || got.Playlists[0].Name != "All favorited tracks" || got.Playlists[0].URI != "library://playlist/1" ||
			got.Playlists[1].Name != "500 Random tracks (from library)" || got.Playlists[1].ImageURL != "https://i.scdn.co/image/abc" {
			t.Errorf("playlists: %+v", got.Playlists)
		}
	})

	t.Run("devices are served pre-session incl. the synthetic group entry", func(t *testing.T) {
		var got struct {
			Devices []MaDevice `json:"devices"`
		}
		if code := getJSON("/ma/devices", &got); code != 200 {
			t.Fatalf("GET /ma/devices: status %d, want 200 pre-session", code)
		}
		want := map[string]bool{"wiim_uuid:A": false, "wiim_uuid:B": false, "solo-1": false, "group:wiim_uuid:A": false}
		for _, d := range got.Devices {
			if _, ok := want[d.Id]; ok {
				want[d.Id] = true
			}
		}
		for id, seen := range want {
			if !seen {
				t.Errorf("devices missing %q: %+v", id, got.Devices)
			}
		}
	})

	t.Run("playlist items are served pre-session with the full track mapping", func(t *testing.T) {
		var got struct {
			Tracks []MaTrack `json:"tracks"`
		}
		if code := getJSON("/ma/playlist-items?id=61", &got); code != 200 {
			t.Fatalf("GET /ma/playlist-items: status %d, want 200 pre-session", code)
		}
		if len(got.Tracks) != 1 {
			t.Fatalf("tracks: %+v", got.Tracks)
		}
		tr := got.Tracks[0]
		if tr.URI != "spotify--x://track/abc" || tr.Name != "Glide" || tr.Album != "The World Is Yours" ||
			len(tr.Artists) != 1 || tr.Artists[0] != "NEIKED" || tr.Position != 3 || tr.DurationMs != 214000 {
			t.Errorf("track mapping: %+v", tr)
		}
	})

	t.Run("recent is served pre-session with the clamped limit forwarded", func(t *testing.T) {
		var got struct {
			Tracks []MaTrack `json:"tracks"`
		}
		if code := getJSON("/ma/recent?limit=5", &got); code != 200 {
			t.Fatalf("GET /ma/recent: status %d, want 200 pre-session", code)
		}
		if len(got.Tracks) != 1 || got.Tracks[0].URI != "spotify--x://track/t9" {
			t.Errorf("recent tracks: %+v", got.Tracks)
		}
		var lim any
		for _, e := range *envs {
			if c, _ := e["command"].(string); c == "music/recently_played_items" {
				args, _ := e["args"].(map[string]any)
				lim = args["limit"]
			}
		}
		if lim != float64(5) {
			t.Errorf("recently_played_items limit: got %v want 5", lim)
		}
	})

	t.Run("a non-MA playback source keeps the 503 retry contract", func(t *testing.T) {
		stub.blob = maTestSettings(masrv.URL, "spotify")
		var got map[string]any
		code := getJSON("/ma/playlists", &got)
		if code != 503 {
			t.Fatalf("GET /ma/playlists with playbackSource=spotify: status %d, want 503", code)
		}
		if got["error"] != "player session not ready" {
			t.Errorf("503 body: %+v", got)
		}
	})
}

// maTestQueueItemsJSON is the live 2.10.7 player_queues/items shape for a
// tail fetch: one fully-mapped media item (top-level image object), one
// non-media name-only entry and one garbage entry that carries neither - it
// must be skipped by the mapping. Item index fields are RELATIVE to the
// requested offset; the daemon adds them back for absolute positions.
const maTestQueueItemsJSON = `[
  {"available":true,"duration":214,"index":0,"queue_id":"wiim_uuid:A","queue_item_id":"qi-n1",
   "name":"A One - Next One","sort_index":0,
   "media_item":{"item_id":"i-n1","provider":"spotify--x","name":"Next One","uri":"spotify--x://track/n1",
     "media_type":"track","artists":[{"name":"A One"}],
     "album":{"item_id":"al-n1","name":"The Album Obj","uri":"spotify--x://album/al-n1"},"duration":214},
   "image":{"type":"thumb","path":"https://i.scdn.co/image/q1","provider":"spotify--x","remotely_accessible":true}},
  {"available":true,"duration":190,"index":1,"queue_id":"wiim_uuid:A","queue_item_id":"qi-n2",
   "name":"Local Song","sort_index":1,"image":null},
  {"available":false,"index":2,"queue_id":"wiim_uuid:A","queue_item_id":"qi-n3","name":"","media_item":null}
]`

// TestMaUpcomingTracks pins the live queue-tail enumeration (issue #138
// defect C): player_queues/items starting one past the current position -
// the SAME enumeration MA's own webui renders (the round-1 source-based
// reconstruction is gone: autoplay re-queues diverge from their source, and
// shuffled/ended queues yielded nothing). TrackId is the ABSOLUTE queue
// position (the play_index target); the fetch is cached per (queue id |
// current index | item count | current uri), so a track change or an autoplay
// append both invalidate; a failed fetch keeps the last good list; no queue /
// no current index carries over without fetching.
func TestMaUpcomingTracks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	clientOf := func(p *AppPlayer) *ma.Client {
		cfg, _ := p.maResolved()
		return p.maClientFor(cfg)
	}
	itemsServer := func(t *testing.T) (*httptest.Server, *[]map[string]any) {
		t.Helper()
		return newMaRecordingServer(t, map[string]string{"player_queues/items": maTestQueueItemsJSON})
	}
	newQueue := func(cur int) *ma.PlayerQueue {
		return &ma.PlayerQueue{QueueID: "wiim_uuid:A", Items: 8, CurrentIndex: new(int(cur))}
	}
	itemsCalls := func(envs *[]map[string]any) int {
		n := 0
		for _, e := range *envs {
			if c, _ := e["command"].(string); c == "player_queues/items" {
				n++
			}
		}
		return n
	}

	t.Run("live queue tail maps to upcoming with absolute TrackIds", func(t *testing.T) {
		srv, envs := itemsServer(t)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		pl := maPlayingPlayer() // current track at index 3 (uri spotify--x://track/abc)

		got := p.maUpcomingTracks(ctx, clientOf(p), pl, newQueue(3))

		// the garbage entry is skipped; absolute positions = cur+1+i
		if len(got) != 2 {
			t.Fatalf("want 2 upcoming tracks (garbage entry skipped), got %d: %+v", len(got), got)
		}
		g0 := got[0]
		if g0.TrackId != "4" || g0.Uri != "spotify--x://track/n1" || g0.Name != "Next One" ||
			g0.Artist != "A One" || g0.Album != "The Album Obj" || g0.ImageUrl != "https://i.scdn.co/image/q1" {
			t.Errorf("upcoming 0: %+v", g0)
		}
		if got[1].TrackId != "5" || got[1].Name != "Local Song" || got[1].Uri != "" || got[1].Album != "" || got[1].ImageUrl != "" {
			t.Errorf("upcoming 1 (name-only entry): %+v", got[1])
		}
		args := envArgs(t, *envs, "player_queues/items")
		if args["queue_id"] != "wiim_uuid:A" || args["limit"] != float64(QueueLimit) || args["offset"] != float64(4) {
			t.Errorf("items args: %+v", args)
		}
	})

	t.Run("cache keyed by (queue id | current index | item count | current uri)", func(t *testing.T) {
		srv, envs := itemsServer(t)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		pl := maPlayingPlayer()

		first := p.maUpcomingTracks(ctx, clientOf(p), pl, newQueue(3))
		got := p.maUpcomingTracks(ctx, clientOf(p), pl, newQueue(3)) // identical poll: 1/s cadence must not re-fetch

		if len(first) != 2 || len(got) != len(first) {
			t.Fatalf("cache hit must keep the list: %v -> %v", first, got)
		}
		if n := itemsCalls(envs); n != 1 {
			t.Errorf("second identical poll must not re-fetch, got %d items calls", n)
		}
	})

	t.Run("track change (index + uri) re-fetches", func(t *testing.T) {
		srv, envs := itemsServer(t)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		pl := maPlayingPlayer()
		_ = p.maUpcomingTracks(ctx, clientOf(p), pl, newQueue(3)) // fetch #1 (offset 4)

		pl.CurrentMedia.URI = "spotify--x://track/n1" // the first upcoming track is now current
		got := p.maUpcomingTracks(ctx, clientOf(p), pl, newQueue(4))
		if len(got) != 2 || got[0].TrackId != "5" {
			t.Errorf("advanced tail must shift the absolute positions: %+v", got)
		}
		if n := itemsCalls(envs); n != 2 {
			t.Errorf("track change must re-fetch, got %d calls", n)
		}
	})

	t.Run("autoplay append (item count bump) re-fetches", func(t *testing.T) {
		srv, envs := itemsServer(t)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		pl := maPlayingPlayer()
		_ = p.maUpcomingTracks(ctx, clientOf(p), pl, newQueue(3)) // count 8

		q := newQueue(3) // same position, same track: ONLY the item count moved
		q.Items = 12     // autoplay batch appended to the queue behind us
		got := p.maUpcomingTracks(ctx, clientOf(p), pl, q)
		if len(got) != 2 {
			t.Errorf("append must yield the fetched tail: %+v", got)
		}
		if n := itemsCalls(envs); n != 2 {
			t.Errorf("item-count change must re-fetch, got %d calls", n)
		}
	})

	t.Run("failed items fetch keeps the last good list", func(t *testing.T) {
		routes := map[string]string{"player_queues/items": maTestQueueItemsJSON}
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		pl := maPlayingPlayer()

		first := p.maUpcomingTracks(ctx, clientOf(p), pl, newQueue(3))
		if len(first) != 2 {
			t.Fatalf("first fetch: %+v", first)
		}
		delete(routes, "player_queues/items") // the fake reads routes per request: next poll's fetch 500s

		// the track advanced between polls (new key -> no cache hit), but the
		// fetch itself now fails: keep the last good list, retry next tick.
		pl.CurrentMedia.URI = "spotify--x://track/n1"
		got := p.maUpcomingTracks(ctx, clientOf(p), pl, newQueue(4))
		if len(got) != 2 || got[0].TrackId != "4" {
			t.Errorf("failure must keep the last good list, got %+v", got)
		}
		if n := itemsCalls(envs); n != 2 {
			t.Errorf("the failing poll must still attempt the fetch (retry next tick), got %d calls", n)
		}
	})

	t.Run("no queue or no current index carries over without fetching", func(t *testing.T) {
		srv, envs := itemsServer(t)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		pl := maPlayingPlayer()

		if got := p.maUpcomingTracks(ctx, clientOf(p), pl, nil); len(got) != 0 {
			t.Errorf("no queue and no cache must yield empty, got %+v", got)
		}

		first := p.maUpcomingTracks(ctx, clientOf(p), pl, newQueue(3)) // prime: fetch #1
		if len(first) != 2 {
			t.Fatalf("prime: %+v", first)
		}
		if got := p.maUpcomingTracks(ctx, clientOf(p), pl, nil); len(got) != 2 || got[0].TrackId != "4" { // transient player_queues/get failure
			t.Errorf("nil queue must carry over the list, got %+v", got)
		}
		noIdx := newQueue(3)
		noIdx.CurrentIndex = nil // queue without a position
		if got := p.maUpcomingTracks(ctx, clientOf(p), pl, noIdx); len(got) != 2 {
			t.Errorf("missing current_index must carry over the list, got %+v", got)
		}
		if n := itemsCalls(envs); n != 1 {
			t.Errorf("carry-over must not fetch, got %d calls", n)
		}
	})
}

// TestMaTrackItem_AlbumShapes pins the RC4 wire-shape regression (issue
// #138): MA 2.10.7 sends the track album as an OBJECT in playlist/queue media
// payloads and as a plain string elsewhere - one batch may mix both, and a
// single object-shaped field used to break the ENTIRE unmarshal of the list.
func TestMaTrackItem_AlbumShapes(t *testing.T) {
	t.Parallel()
	batch := `[
	  {"item_id":"t1","name":"Glide","uri":"spotify--x://track/one",
	   "album":"The World Is Yours","artists":[{"name":"NEIKED"}],"position":0,
	   "image":{"type":"thumb","path":"https://i.scdn.co/image/a","provider":"p","remotely_accessible":true}},
	  {"item_id":"t2","name":"Cobalt","uri":"spotify--x://track/two","position":1,
	   "album":{"item_id":"al1","name":"Polychrome","uri":"spotify--x://album/al1"},
	   "metadata":{"images":[{"type":"thumb","path":"https://i.scdn.co/image/b","provider":"p","remotely_accessible":true}]}}
	]`
	var tr []ma.TrackItem
	if err := json.Unmarshal([]byte(batch), &tr); err != nil {
		t.Fatalf("mixed album shapes must decode: %v", err)
	}
	if len(tr) != 2 {
		t.Fatalf("want 2 tracks, got %d", len(tr))
	}
	if tr[0].Album.Name() != "The World Is Yours" {
		t.Errorf("string album: %q", tr[0].Album.Name())
	}
	if tr[1].Album.Name() != "Polychrome" {
		t.Errorf("object album must decode to its name, got %q", tr[1].Album.Name())
	}
	// cover: top-level image first, metadata.images as fallback
	if url := maImageURL(tr[0].Cover()); url != "https://i.scdn.co/image/a" {
		t.Errorf("top-level cover: %q", url)
	}
	if url := maImageURL(tr[1].Cover()); url != "https://i.scdn.co/image/b" {
		t.Errorf("metadata cover: %q", url)
	}
}

// TestMaPollTick_GroupSelfHeal pins the observer self-heal (issue #138): if
// the selected player becomes a firmware slave during polling (HA formed the
// WiiM group around playback), the next tick retargets to its master before
// touching the queue or applying any state. Since the defect-B warmup runs on
// every MA-configured tick, this tick ALSO kicks exactly one (failed - the
// fake routes no browse) background playlist warmup; it is joined before the
// command assertion (multiset: tick requests and warmup browses race in order).
func TestMaPollTick_GroupSelfHeal(t *testing.T) {
	t.Parallel()
	slave := `{
	  "player_id":"wiim_uuid:A","name":"WiiM Kueche","available":true,"playback_state":"playing",
	  "group_members":[],"synced_to":"wiim_uuid:M"
	}`
	srv, envs := newMaRecordingServer(t, map[string]string{"players/get": slave})
	p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
	p.maActivePlayerId = "wiim_uuid:A"

	p.maPollTick(context.Background())
	waitMaWarmupDone(t, p) // join the background warmup before asserting on commands

	if p.maActivePlayerId != "wiim_uuid:M" {
		t.Errorf("tick must retarget to the master, got %q", p.maActivePlayerId)
	}
	assertCommandSet(t, envCommandKeys(*envs), []string{"players/get", "music/browse@root"}) // root browse 500s (unrouted) -> single failed warmup; no queue fetch on the heal tick
	if p.state.remoteState != nil {
		t.Errorf("heal tick must not apply state, got %+v", p.state.remoteState)
	}
}

// maTestPlayerPlayingJSON is a live-shaped players/get answer for poll-tick
// tests that need an ACTIVE (non-idle) selection on wiim_uuid:A - active so
// the tick skips the idle playlog fetch and also exercises the queue get.
const maTestPlayerPlayingJSON = `{
  "player_id":"wiim_uuid:A","name":"WiiM Wohnzimmer","available":true,"playback_state":"playing",
  "elapsed_time":42.5,"elapsed_time_last_updated":1700000000,"volume_level":80,
  "current_media":{"uri":"spotify--x://track/abc","media_type":"track","title":"Glide","artist":"NEIKED/Portugal. The Man","album":"The World Is Yours","image_url":"","duration":184}
}`

// TestMaPlaylistWarmup pins the defect-B background warmup (issue #138): a
// STALE playlist cache (or an empty one) is refreshed in the BACKGROUND with
// the generous warmup budget - root + every provider folder exactly once - so
// a cold browse neither blocks the 1/s poll nor dies on maControlTimeout. A
// fresh cache (TTL) skips the round; a failed browse leaves the cache empty
// (never an authoritative list, defect 4).
func TestMaPlaylistWarmup(t *testing.T) {
	t.Parallel()
	browseRoutes := map[string]string{
		"music/browse@root":                          maTestBrowseRoot,
		"music/browse@builtin://playlists":           maTestBrowseBuiltinPlaylists,
		"music/browse@spotify--yofvZfhZ://playlists": maTestBrowseSpotifyPlaylists,
	}

	t.Run("warmup browses root and every provider folder once", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, browseRoutes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		cfg, _ := p.maResolved()

		p.maMaybeWarmPlaylists(p.maClientFor(cfg))
		waitMaWarmupDone(t, p)

		// ambient_sounds://playlists is unrouted -> 500 -> folder skipped
		assertCommandSeq(t, envCommandKeys(*envs), []string{
			"music/browse@root",
			"music/browse@builtin://playlists",
			"music/browse@ambient_sounds://playlists",
			"music/browse@spotify--yofvZfhZ://playlists",
		})
		p.maPlaylistMu.Lock()
		cache := p.maPlaylistCache
		fresh := time.Since(p.maPlaylistCachedAt) < maPlaylistTTL
		p.maPlaylistMu.Unlock()
		if len(cache) != 3 {
			t.Fatalf("want 3 warmed playlists, got %d: %+v", len(cache), cache)
		}
		if cache[0].ItemID != "1" || cache[2].ItemID != "61" || cache[2].URI != "library://playlist/61" {
			t.Errorf("warmed cache content: %+v", cache)
		}
		if !fresh {
			t.Error("warmup must stamp the cache as fresh")
		}
	})

	t.Run("poll tick kicks the warmup in the background", func(t *testing.T) {
		routes := map[string]string{"players/get": maTestPlayerPlayingJSON}
		for k, v := range browseRoutes {
			routes[k] = v
		}
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		p.maActivePlayerId = "wiim_uuid:A"

		p.maPollTick(context.Background())
		waitMaWarmupDone(t, p)

		// the tick's own requests and the goroutine's browses race in order -
		// pin the SET: one players/get, one (failed, unrouted) queue get, one
		// full browse round, nothing else
		assertCommandSet(t, envCommandKeys(*envs), []string{
			"players/get",
			"player_queues/get",
			"music/browse@root",
			"music/browse@builtin://playlists",
			"music/browse@ambient_sounds://playlists",
			"music/browse@spotify--yofvZfhZ://playlists",
		})
		p.maPlaylistMu.Lock()
		n := len(p.maPlaylistCache)
		p.maPlaylistMu.Unlock()
		if n != 3 {
			t.Errorf("warmup must populate the cache, got %d entries", n)
		}

		before := len(*envs)
		p.maPollTick(context.Background()) // fresh cache (TTL gate) -> no second round
		waitMaWarmupDone(t, p)
		assertCommandSeq(t, envCommands((*envs)[before:]), []string{"players/get", "player_queues/get"})
	})

	t.Run("failed root browse leaves an empty cache after one attempt", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, map[string]string{"players/get": maTestPlayerPlayingJSON})
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		p.maActivePlayerId = "wiim_uuid:A"

		p.maPollTick(context.Background())
		waitMaWarmupDone(t, p)

		assertCommandSet(t, envCommandKeys(*envs), []string{
			"players/get", "player_queues/get", "music/browse@root", // root 500 -> the only browse
		})
		p.maPlaylistMu.Lock()
		n := len(p.maPlaylistCache)
		p.maPlaylistMu.Unlock()
		if n != 0 {
			t.Errorf("failed warmup must not cache anything, got %d entries", n)
		}
	})
}

// ---------- helpers for the part-2 tests ----------

// newMaRecordingServer routes canned per-command responses and records every
// request envelope in order; unrouted commands answer 500 (like a provider
// without that collection live). The append is mutex-guarded because one
// tick can fan out CONCURRENT requests against this server: the background
// playlist warmup (issue #138 defect B) browses while the tick itself does
// players/get + player_queues/get on the same URL (issue #138).
func newMaRecordingServer(t *testing.T, routes map[string]string) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	envs := []map[string]any{}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var env map[string]any
		_ = json.Unmarshal(b, &env)
		key, _ := env["command"].(string)
		if key == "music/browse" {
			args, _ := env["args"].(map[string]any)
			path, _ := args["path"].(string)
			key = "music/browse@" + path
		}
		mu.Lock()
		envs = append(envs, env)
		mu.Unlock()
		if body, ok := routes[key]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv, &envs
}

func envCommands(envs []map[string]any) []string {
	out := make([]string, 0, len(envs))
	for _, e := range envs {
		c, _ := e["command"].(string)
		out = append(out, c)
	}
	return out
}

// envCommandKeys is envCommands with music/browse disambiguated by its path
// argument (the same keying the fake servers use for routing): needed where
// one test issues several browses of different paths.
func envCommandKeys(envs []map[string]any) []string {
	out := make([]string, 0, len(envs))
	for _, e := range envs {
		c, _ := e["command"].(string)
		if c == "music/browse" {
			args, _ := e["args"].(map[string]any)
			path, _ := args["path"].(string)
			c = "music/browse@" + path
		}
		out = append(out, c)
	}
	return out
}

func assertOnlyCommands(t *testing.T, envs []map[string]any, want []string) {
	t.Helper()
	got := envCommands(envs)
	if len(got) != len(want) {
		t.Fatalf("commands: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d: got %q want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func envArgs(t *testing.T, envs []map[string]any, command string) map[string]any {
	t.Helper()
	for _, e := range envs {
		if c, _ := e["command"].(string); c == command {
			args, _ := e["args"].(map[string]any)
			return args
		}
	}
	t.Fatalf("command %q never sent (got %v)", command, envCommands(envs))
	return nil
}

// waitMaWarmupDone blocks until the background playlist warmup finishes -
// maPlaylistWarming flips back to false on BOTH a successful and a failed
// browse, so the join works for either outcome. Canned-fake browses take
// milliseconds; a timeout means the goroutine hung or leaked. Joining BEFORE
// asserting on recorded commands is required: an in-flight warmup may still
// append its music/browse requests (issue #138 defect B).
func waitMaWarmupDone(t *testing.T, p *AppPlayer) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.maPlaylistMu.Lock()
		warming := p.maPlaylistWarming
		p.maPlaylistMu.Unlock()
		if !warming {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("playlist warmup did not finish within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertCommandSeq pins an EXACT ordered command sequence.
func assertCommandSeq(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("commands: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d: got %q want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// assertCommandSet pins a command MULTISET (duplicates kept) - for groups
// whose mutual order is not pinned, e.g. the poll tick's own requests racing
// the background warmup's browses.
func assertCommandSet(t *testing.T, got []string, want []string) {
	t.Helper()
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if len(g) != len(w) {
		t.Fatalf("command set: got %v want %v", g, w)
	}
	for i := range g {
		if g[i] != w[i] {
			t.Fatalf("command set: got %v want %v", g, w)
		}
	}
}
