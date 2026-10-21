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

// TestApplyMaState_IdleClearsStaleMedia: live MA players keep a non-null
// (stale) current_media after going idle, so PlaybackState gates first;
// an idle poll must clear the observed state and release ownership.
func TestApplyMaState_IdleClearsStaleMedia(t *testing.T) {
	t.Parallel()
	p := newMaTestPlayer(t, nil)
	p.applyMaState(maPlayingPlayer(), nil, nil) // seed a playing state
	if p.state.remoteState == nil {
		t.Fatal("seed failed")
	}

	pl := maPlayingPlayer()
	pl.PlaybackState = "idle" // media intentionally still set (stale)
	p.applyMaState(pl, nil, nil)

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

	p.applyMaState(pl, nil, nil)
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
	})

	t.Run("nil queue carries the previous list over", func(t *testing.T) {
		p := newMaTestPlayer(t, nil)
		q := ma.PlayerQueue{State: "playing"}
		list := []QueueTrack{{TrackId: "4", Uri: "spotify--x://track/one", Name: "One"}}
		p.applyMaState(maPlayingPlayer(), &q, list)

		// transient player_queues/get failure (queue nil) while media is
		// still active on the same device: keep the pane, don't blank it
		p.applyMaState(maPlayingPlayer(), nil, nil)

		got := p.state.remoteState.NextTracks
		if len(got) != 1 || got[0].TrackId != "4" {
			t.Errorf("carry-over failed: %+v", got)
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
// (issue #138): context+offset -> play_media with start_item, bare uri in
// the observed queue -> play_index (queue-preserving skip), anything else
// -> single-item context, no uri -> resume semantics.
func TestMaSend_PlayUriRouting(t *testing.T) {
	t.Parallel()
	srv, last := newMaFakeServer(t)
	p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
	p.maActivePlayerId = "wiim_uuid:A"
	ctx := context.Background()

	t.Run("uri with offset uri starts the source at that item", func(t *testing.T) {
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

	t.Run("bare uri matching an observed upcoming track skips in queue", func(t *testing.T) {
		p.state.remoteState = &RemoteState{
			DeviceId:   "wiim_uuid:A",
			NextTracks: []QueueTrack{{TrackId: "qi-7", Uri: "spotify--x://track/abc"}},
		}
		if err := p.maSend(ctx, "play", ApiRequestDataPlay{Uri: "spotify--x://track/abc"}); err != nil {
			t.Fatalf("maSend(play): %v", err)
		}
		env := *last
		if env["command"] != "player_queues/play_index" {
			t.Fatalf("command: got %v want player_queues/play_index", env["command"])
		}
		args, _ := env["args"].(map[string]any)
		if args["queue_id"] != "wiim_uuid:A" || args["index"] != "qi-7" {
			t.Errorf("args: %+v", args)
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

	t.Run("play without uri keeps resume semantics", func(t *testing.T) {
		if err := p.maSend(ctx, "play", ApiRequestDataPlay{}); err != nil {
			t.Fatalf("maSend(play): %v", err)
		}
		env := *last
		if env["command"] != "players/cmd/play" {
			t.Fatalf("command: got %v want players/cmd/play", env["command"])
		}
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

// maTestRecentItems mirrors the play-log mapping: lightweight entries, and
// a non-track row that must be filtered out by the client.
const maTestRecentItems = `[
  {"item_id":"t9","provider":"spotify--x","name":"Recently One","uri":"spotify--x://track/r1","media_type":"track",
   "image":{"type":"thumb","path":"https://i.scdn.co/image/r1","provider":"spotify--yofvZfhZ","remotely_accessible":true}},
  {"item_id":"a1","provider":"spotify--x","name":"Some Album","uri":"spotify--x://album/album","media_type":"album"},
  {"item_id":"t8","provider":"local","name":"Recently Two","uri":"","media_type":"track"}
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
// skipped, empty list on unconfigured/unreachable - never an error.
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

	t.Run("unreachable yields empty list", func(t *testing.T) {
		srv, _ := newMaContentFakeServer(t, routes)
		url := srv.URL
		srv.Close()

		p := newMaTestPlayer(t, maTestSettings(url, "ma"))
		payload, err := p.maListPlaylists(context.Background())
		if err != nil {
			t.Fatalf("unreachable must not error: %v", err)
		}
		if pl := playlistsFromPayload(t, payload); len(pl) != 0 {
			t.Errorf("want empty list, got %+v", pl)
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
// empty list on failure, limit clamping.
func TestMaListRecent(t *testing.T) {
	t.Parallel()

	t.Run("keeps playable track rows only", func(t *testing.T) {
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
		tracks := tracksFromPayload(t, payload)
		if len(tracks) != 1 {
			t.Fatalf("want 1 track (album row + uri-less row filtered), got %d: %+v", len(tracks), tracks)
		}
		if tracks[0].Name != "Recently One" || tracks[0].URI != "spotify--x://track/r1" ||
			tracks[0].ImageURL != "https://i.scdn.co/image/r1" {
			t.Errorf("track 0: %+v", tracks[0])
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

// TestMaTransferPlayback pins the user-approved transfer semantics
// (issue #138): selecting a DIFFERENT target while the old one plays moves
// the music - replay the same source on the new player from the current
// track, then pause the old one. Nothing playing = plain re-target; a
// failed replay aborts the selection and keeps the old target.
func TestMaTransferPlayback(t *testing.T) {
	t.Parallel()

	t.Run("playing playlist queue moves to the new target", func(t *testing.T) {
		routes := map[string]string{"players/all": maTestGroupPlayersJSON, "player_queues/get": maTestQueuePlayingPlaylist, "player_queues/play_media": `{}`}
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		p.maActivePlayerId = "wiim_uuid:A"

		if _, err := p.maSelectPlayer(context.Background(), "solo-1"); err != nil {
			t.Fatalf("maSelectPlayer: %v", err)
		}
		if p.maActivePlayerId != "solo-1" {
			t.Errorf("selection: %q", p.maActivePlayerId)
		}
		assertOnlyCommands(t, *envs, []string{"players/all", "player_queues/get", "player_queues/play_media", "players/cmd/pause"})

		pm := envArgs(t, *envs, "player_queues/play_media")
		if pm["queue_id"] != "solo-1" || pm["option"] != ma.QueueOptionReplace {
			t.Errorf("play_media args: %+v", pm)
		}
		media, _ := pm["media"].([]any)
		if len(media) != 1 || media[0] != "library://playlist/61" {
			t.Errorf("media must be the full source: %v", pm["media"])
		}
		if pm["start_item"] != "item-1" {
			t.Errorf("start_item must be the current track: %v", pm["start_item"])
		}
		pause := envArgs(t, *envs, "players/cmd/pause")
		if pause["player_id"] != "wiim_uuid:A" {
			t.Errorf("pause args: %+v", pause)
		}
	})

	t.Run("single-item queue replays the bare track uri", func(t *testing.T) {
		routes := map[string]string{"players/all": maTestGroupPlayersJSON, "player_queues/get": maTestQueuePlayingSingle, "player_queues/play_media": `{}`}
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		p.maActivePlayerId = "wiim_uuid:A"

		if _, err := p.maSelectPlayer(context.Background(), "solo-1"); err != nil {
			t.Fatalf("maSelectPlayer: %v", err)
		}
		pm := envArgs(t, *envs, "player_queues/play_media")
		media, _ := pm["media"].([]any)
		if len(media) != 1 || media[0] != "spotify--x://track/abc" {
			t.Errorf("media must be the bare track uri: %v", pm["media"])
		}
		if _, ok := pm["start_item"]; ok {
			t.Errorf("no start_item expected for a single-item queue: %+v", pm)
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

	t.Run("failed replay aborts the selection and keeps the old target", func(t *testing.T) {
		routes := map[string]string{"players/all": maTestGroupPlayersJSON, "player_queues/get": maTestQueuePlayingPlaylist}
		srv, _ := newMaRecordingServer(t, routes) // play_media unrouted -> 500
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		p.maActivePlayerId = "wiim_uuid:A"

		_, err := p.maSelectPlayer(context.Background(), "solo-1")
		if err == nil {
			t.Fatal("failed replay must error")
		}
		if errors.Is(err, ErrBadRequest) {
			t.Errorf("a server failure must not look like a client-side 400: %v", err)
		}
		if p.maActivePlayerId != "wiim_uuid:A" {
			t.Errorf("old selection must be kept, got %q", p.maActivePlayerId)
		}
	})
}

// TestMaUpcomingTracks pins the queue-tail reconstruction (issue #138):
// single-source queue -> everything after the current track in source order,
// TrackId = absolute queue index (the play_index target); cache per (source,
// current track); shuffle/multi-source -> honest empty; fetch failure keeps
// the last good list.
func TestMaUpcomingTracks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	routes := map[string]string{"music/playlists/playlist_tracks": maTestPlaylistTracks}

	clientOf := func(p *AppPlayer) *ma.Client {
		cfg, _ := p.maResolved()
		return p.maClientFor(cfg)
	}
	playlistQueue := func(shuffle bool, sources int) *ma.PlayerQueue {
		srcs := []ma.QueueSource{{ItemID: "61", Provider: "library", Name: "257ers", URI: "library://playlist/61", MediaType: "playlist"}}
		if sources == 2 {
			srcs = append(srcs, ma.QueueSource{ItemID: "tX", Provider: "spotify--x", Name: "Solo", URI: "spotify--x://track/solo", MediaType: "track"})
		}
		return &ma.PlayerQueue{CurrentIndex: new(int(0)), ShuffleEnabled: shuffle, Sources: srcs}
	}

	t.Run("queue tail after the current track with queue-index TrackIds", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		pl := maPlayingPlayer() // CurrentMedia uri = t1 (index 0)

		got := p.maUpcomingTracks(ctx, clientOf(p), pl, playlistQueue(false, 1))

		// t2 "Local Jam" survives; t3 (no uri, no name) is dropped
		if len(got) != 1 {
			t.Fatalf("want 1 upcoming track, got %d: %+v", len(got), got)
		}
		g := got[0]
		if g.TrackId != "1" || g.Uri != "local://track/jam" || g.Name != "Local Jam" || g.ImageUrl != "" {
			t.Errorf("upcoming 0: %+v", g)
		}
		calls := 0
		for _, e := range *envs {
			if e["command"] == "music/playlists/playlist_tracks" {
				calls++
			}
		}
		if calls != 1 {
			t.Errorf("playlist_tracks calls: got %d want 1", calls)
		}
	})

	t.Run("caches per (source, current track)", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		pl := maPlayingPlayer()
		q := playlistQueue(false, 1)
		first := p.maUpcomingTracks(ctx, clientOf(p), pl, q)

		got := p.maUpcomingTracks(ctx, clientOf(p), pl, q)
		if len(got) != len(first) {
			t.Fatalf("cache hit changed the list: %v -> %v", first, got)
		}
		calls := 0
		for _, e := range *envs {
			if e["command"] == "music/playlists/playlist_tracks" {
				calls++
			}
		}
		if calls != 1 {
			t.Errorf("second poll must not re-fetch, got %d calls", calls)
		}
	})

	t.Run("track change recomputes the tail", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		pl := maPlayingPlayer()
		q := playlistQueue(false, 1)
		_ = p.maUpcomingTracks(ctx, clientOf(p), pl, q) // t1 -> fetch #1

		pl.CurrentMedia.URI = "local://track/jam" // t2 is now current: tail = [t3] -> dropped -> empty
		got := p.maUpcomingTracks(ctx, clientOf(p), pl, q)
		if len(got) != 0 {
			t.Errorf("tail after the second-to-last track must be empty, got %+v", got)
		}
		calls := 0
		for _, e := range *envs {
			if e["command"] == "music/playlists/playlist_tracks" {
				calls++
			}
		}
		if calls != 2 {
			t.Errorf("track change must re-fetch, got %d calls", calls)
		}
	})

	t.Run("shuffle on yields empty without fetching", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		got := p.maUpcomingTracks(ctx, clientOf(p), maPlayingPlayer(), playlistQueue(true, 1))
		if len(got) != 0 {
			t.Errorf("shuffled queue must be empty, got %+v", got)
		}
		if len(*envs) != 0 {
			t.Errorf("no command may be sent for a shuffled queue: %v", *envs)
		}
	})

	t.Run("multi-source queue yields empty without fetching", func(t *testing.T) {
		srv, envs := newMaRecordingServer(t, routes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		got := p.maUpcomingTracks(ctx, clientOf(p), maPlayingPlayer(), playlistQueue(false, 2))
		if len(got) != 0 {
			t.Errorf("multi-source queue must be empty, got %+v", got)
		}
		if len(*envs) != 0 {
			t.Errorf("no command may be sent for a mixed queue: %v", *envs)
		}
	})

	t.Run("fetch failure keeps the last good list", func(t *testing.T) {
		srv, _ := newMaRecordingServer(t, nil) // playlist_tracks unrouted -> 500
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		pl := maPlayingPlayer()
		q := playlistQueue(false, 1)
		// prime the cache as a previous successful poll would have
		p.maUpcomingKey = "library://playlist/61|spotify--x://track/one"
		p.maUpcomingCached = []QueueTrack{{TrackId: "1", Uri: "local://track/jam", Name: "Local Jam"}}

		got := p.maUpcomingTracks(ctx, clientOf(p), pl, q)
		if len(got) != 1 || got[0].Name != "Local Jam" {
			t.Errorf("failure must keep the last good list, got %+v", got)
		}
	})

	t.Run("album source calls album_tracks with provider args", func(t *testing.T) {
		albumRoutes := map[string]string{"music/albums/album_tracks": `[
		  {"item_id":"at1","name":"A-One","uri":"spotify--x://track/aa","media_type":"track",
		   "album":{"name":"The Album"},"position":0},
		  {"item_id":"at2","name":"A-Two","uri":"spotify--x://track/bb","media_type":"track",
		   "album":{"name":"The Album"},"position":1}
		]`}
		srv, envs := newMaRecordingServer(t, albumRoutes)
		p := newMaTestPlayer(t, maTestSettings(srv.URL, "ma"))
		pl := maPlayingPlayer()
		pl.CurrentMedia.URI = "spotify--x://track/aa" // at1 current -> tail [A-Two]
		q := &ma.PlayerQueue{
			CurrentIndex: new(int(0)),
			Sources:      []ma.QueueSource{{ItemID: "al9", Provider: "spotify--x", Name: "The Album", URI: "spotify--x://album/al9", MediaType: "album"}},
		}

		got := p.maUpcomingTracks(ctx, clientOf(p), pl, q)

		if len(got) != 1 || got[0].Name != "A-Two" || got[0].TrackId != "1" || got[0].Album != "The Album" {
			t.Errorf("album tail: %+v", got)
		}
		args := envArgs(t, *envs, "music/albums/album_tracks")
		if args["item_id"] != "al9" || args["provider_instance_id_or_domain"] != "spotify--x" {
			t.Errorf("album_tracks args: %+v", args)
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
// touching the queue or applying any state.
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

	if p.maActivePlayerId != "wiim_uuid:M" {
		t.Errorf("tick must retarget to the master, got %q", p.maActivePlayerId)
	}
	assertOnlyCommands(t, *envs, []string{"players/get"}) // no queue fetch on the heal tick
	if p.state.remoteState != nil {
		t.Errorf("heal tick must not apply state, got %+v", p.state.remoteState)
	}
}

// ---------- helpers for the part-2 tests ----------

// newMaRecordingServer routes canned per-command responses and records every
// request envelope in order; unrouted commands answer 500 (like a provider
// without that collection live).
func newMaRecordingServer(t *testing.T, routes map[string]string) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	envs := []map[string]any{}
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
		envs = append(envs, env)
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
