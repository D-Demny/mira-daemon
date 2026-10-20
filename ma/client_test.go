package ma

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCall_WireShape pins the envelope: single POST to /api with command,
// args ({} when nil) and a non-empty message_id, bearer auth header.
func TestCall_WireShape(t *testing.T) {
	var auth, path string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		auth = req.Header.Get("Authorization")
		path = req.URL.Path
		var env map[string]any
		b, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(b, &env)
		if env["command"] != "players/all" || env["message_id"].(string) == "" {
			t.Errorf("envelope: got %v", env)
		}
		args, ok := env["args"].(map[string]any)
		if !ok || len(args) != 0 {
			t.Errorf("nil args must encode as empty object, got %v", env["args"])
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`null`))
	}))
	defer ts.Close()
	client := NewClient(ts.URL, "test-token")
	if _, err := client.Call(context.Background(), "players/all", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if auth != "Bearer test-token" {
		t.Errorf("auth header: got %q want bearer token", auth)
	}
	if path != "/api" {
		t.Errorf("path: got %q want /api", path)
	}
}

func TestCall_401IsUnauthenticated(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "Authentication required")
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "wrong-token")
	_, err := c.Call(context.Background(), "players/all", nil)
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("401 must map to ErrUnauthenticated, got %v", err)
	}
	if gotAuth != "Bearer wrong-token" {
		t.Errorf("token must be sent verbatim: got %q", gotAuth)
	}
}

func TestCall_Non2xxIsAPIError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "t")
	_, err := c.Call(context.Background(), "players/get", map[string]any{"player_id": "p"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.StatusCode != 500 || apiErr.Message != "boom" || apiErr.Command != "players/get" {
		t.Errorf("APIError fields: %+v", apiErr)
	}
}

func TestCall_PlainTextOn200IsCommandFailure(t *testing.T) {
	// MA answers command failures as plain error strings on a 200
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "Unknown command: bogus")
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "t")
	_, err := c.Call(context.Background(), "bogus", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("plain-text 200 must map to *APIError, got %v", err)
	}
	if apiErr.Message != "Unknown command: bogus" || apiErr.StatusCode != 200 {
		t.Errorf("APIError fields: %+v", apiErr)
	}
}

func TestCall_MalformedJSONOn200IsCommandFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{not json`)
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "t")
	if _, err := c.Call(context.Background(), "players/all", nil); !errors.As(err, new(*APIError)) {
		t.Fatalf("malformed-JSON 200 must map to *APIError, got %v", err)
	}
}

func TestCall_EmptyBodyOn200IsSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "t")
	if raw, err := c.Call(context.Background(), "players/cmd/pause", map[string]any{"player_id": "p"}); err != nil || raw != nil {
		t.Fatalf("empty 200 body must be a success with nil result, got (%v, %v)", raw, err)
	}
}

func TestNewClient_TrimsTrailingSlash(t *testing.T) {
	c := NewClient("http://10.0.0.1:8095/", "t")
	if c.baseURL != "http://10.0.0.1:8095" {
		t.Errorf("trailing slash must be trimmed: %q", c.baseURL)
	}
}

const samplePlayersJSON = `[
  {
    "player_id": "wiim_uuid:A",
    "name": "WiiM Wohnzimmer",
    "available": true,
    "playback_state": "playing",
    "elapsed_time": 42.5,
    "elapsed_time_last_updated": 1700000000,
    "volume_level": 80,
    "volume_muted": false,
    "current_media": {
      "uri": "spotify--x://track/abc",
      "media_type": "track",
      "title": "Glide",
      "artist": "NEIKED/Portugal. The Man",
      "album": "The World Is Yours",
      "album_artist": null,
      "image_url": "http://10.0.0.1:8095/imageproxy/x?size=512&fmt=jpg",
      "duration": 184
    }
  },
  {
    "player_id": "upfac71c9e",
    "name": "Unpowered",
    "available": false,
    "playback_state": "idle",
    "elapsed_time": null,
    "elapsed_time_last_updated": null,
    "volume_level": null,
    "volume_muted": null,
    "current_media": null
  }
]`

// TestListPlayers_DecodesNullables pins the nullable wire fields: an
// unavailable player answers null for position/volume/media and must not
// break unmarshalling (live-verified against MA 2.10.7, issue #134).
func TestListPlayers_DecodesNullables(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(samplePlayersJSON))
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "t")
	players, err := c.ListPlayers(context.Background())
	if err != nil {
		t.Fatalf("ListPlayers: %v", err)
	}
	if len(players) != 2 {
		t.Fatalf("want 2 players, got %d", len(players))
	}
	p0 := players[0]
	if p0.PlayerID != "wiim_uuid:A" || !p0.Available || p0.PlaybackState != "playing" {
		t.Errorf("player 0 basics: %+v", p0)
	}
	if p0.ElapsedTime == nil || *p0.ElapsedTime != 42.5 {
		t.Errorf("elapsed_time: %v", p0.ElapsedTime)
	}
	if p0.VolumeLevel == nil || *p0.VolumeLevel != 80 {
		t.Errorf("volume_level: %v", p0.VolumeLevel)
	}
	m := p0.CurrentMedia
	if m == nil || m.Title != "Glide" || m.Duration != 184 || m.ImageURL == "" {
		t.Fatalf("current_media: %+v", m)
	}

	p1 := players[1]
	if p1.Available || p1.ElapsedTime != nil || p1.VolumeLevel != nil || p1.CurrentMedia != nil {
		t.Errorf("unavailable player must keep null fields nil: %+v", p1)
	}
}

// TestGetPlayer_ArgEncode pins the players/get args shape.
func TestGetPlayer_ArgEncode(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &env)
		if env["command"] != "players/get" {
			t.Errorf("command: %v", env["command"])
		}
		args, _ := env["args"].(map[string]any)
		if args["player_id"] != "wiim_uuid:A" {
			t.Errorf("args: %v", args)
		}
		_, _ = w.Write([]byte(`{"player_id":"wiim_uuid:A","name":"WiiM Wohnzimmer","available":true,"playback_state":"idle"}`))
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "t")
	p, err := c.GetPlayer(context.Background(), "wiim_uuid:A")
	if err != nil {
		t.Fatalf("GetPlayer: %v", err)
	}
	if p.PlayerID != "wiim_uuid:A" || p.CurrentMedia != nil {
		t.Errorf("player: %+v", p)
	}
}

// TestControlCommands pins the exact command names and arg shapes of every
// control verb against the live 2.10.7 schemas (issue #134): all
// players/cmd/* take a singular player_id; seek adds position (seconds);
// volume_set takes 0..100; volume_mute requires an explicit muted bool;
// shuffle goes through player_queues/shuffle with queue_id == player id;
// repeat takes the repeat_mode enum.
func TestControlCommands(t *testing.T) {
	cases := []struct {
		name     string
		call     func(c *Client, ctx context.Context) error
		wantCmd  string
		wantArgs map[string]any
	}{
		{"play", func(c *Client, ctx context.Context) error { return c.Play(ctx, "p1") }, "players/cmd/play", map[string]any{"player_id": "p1"}},
		{"pause", func(c *Client, ctx context.Context) error { return c.Pause(ctx, "p1") }, "players/cmd/pause", map[string]any{"player_id": "p1"}},
		{"play_pause", func(c *Client, ctx context.Context) error { return c.PlayPause(ctx, "p1") }, "players/cmd/play_pause", map[string]any{"player_id": "p1"}},
		{"next", func(c *Client, ctx context.Context) error { return c.Next(ctx, "p1") }, "players/cmd/next", map[string]any{"player_id": "p1"}},
		{"previous", func(c *Client, ctx context.Context) error { return c.Previous(ctx, "p1") }, "players/cmd/previous", map[string]any{"player_id": "p1"}},
		{"seek", func(c *Client, ctx context.Context) error { return c.Seek(ctx, "p1", 123) }, "players/cmd/seek", map[string]any{"player_id": "p1", "position": float64(123)}},
		{"volume_set", func(c *Client, ctx context.Context) error { return c.VolumeSet(ctx, "p1", 42) }, "players/cmd/volume_set", map[string]any{"player_id": "p1", "volume_level": float64(42)}},
		{"volume_up", func(c *Client, ctx context.Context) error { return c.VolumeUp(ctx, "p1") }, "players/cmd/volume_up", map[string]any{"player_id": "p1"}},
		{"volume_down", func(c *Client, ctx context.Context) error { return c.VolumeDown(ctx, "p1") }, "players/cmd/volume_down", map[string]any{"player_id": "p1"}},
		{"volume_mute", func(c *Client, ctx context.Context) error { return c.VolumeMute(ctx, "p1", true) }, "players/cmd/volume_mute", map[string]any{"player_id": "p1", "muted": true}},
		{"shuffle", func(c *Client, ctx context.Context) error { return c.Shuffle(ctx, "q1", true) }, "player_queues/shuffle", map[string]any{"queue_id": "q1", "shuffle_enabled": true}},
		{"repeat_one", func(c *Client, ctx context.Context) error { return c.Repeat(ctx, "p1", RepeatOne) }, "players/cmd/repeat", map[string]any{"player_id": "p1", "repeat_mode": "one"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var env map[string]any
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &env)
				if env["command"] != tc.wantCmd {
					t.Errorf("command: got %v want %q", env["command"], tc.wantCmd)
				}
				args, _ := env["args"].(map[string]any)
				for k, want := range tc.wantArgs {
					if args[k] != want {
						t.Errorf("arg %s: got %v want %v", k, args[k], want)
					}
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`null`))
			}))
			defer ts.Close()

			c := NewClient(ts.URL, "t")
			if err := tc.call(c, context.Background()); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
		})
	}
}
