package ma

import (
	"context"
	"encoding/json"
	"fmt"
)

// RepeatMode values for players/cmd/repeat (MA 2.10.7 schema enum).
const (
	RepeatOff     = "off"
	RepeatOne     = "one" // repeat the current item
	RepeatAll     = "all" // repeat the whole queue
	RepeatUnknown = "unknown"
)

// ListPlayers calls players/all and returns every registered player.
func (c *Client) ListPlayers(ctx context.Context) ([]Player, error) {
	raw, err := c.Call(ctx, "players/all", nil)
	if err != nil {
		return nil, err
	}
	var players []Player
	if err := json.Unmarshal(raw, &players); err != nil {
		return nil, fmt.Errorf("ma: decoding players/all result: %w", err)
	}
	return players, nil
}

// GetPlayer calls players/get for one player id.
func (c *Client) GetPlayer(ctx context.Context, playerID string) (*Player, error) {
	raw, err := c.Call(ctx, "players/get", playerArgs(playerID))
	if err != nil {
		return nil, err
	}
	var p Player
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("ma: decoding players/get result for %s: %w", playerID, err)
	}
	return &p, nil
}

// Play sends players/cmd/play (unpause / resume on the player).
func (c *Client) Play(ctx context.Context, playerID string) error {
	_, err := c.Call(ctx, "players/cmd/play", playerArgs(playerID))
	return err
}

// Pause sends players/cmd/pause.
func (c *Client) Pause(ctx context.Context, playerID string) error {
	_, err := c.Call(ctx, "players/cmd/pause", playerArgs(playerID))
	return err
}

// PlayPause sends players/cmd/play_pause (toggle).
func (c *Client) PlayPause(ctx context.Context, playerID string) error {
	_, err := c.Call(ctx, "players/cmd/play_pause", playerArgs(playerID))
	return err
}

// Next sends players/cmd/next.
func (c *Client) Next(ctx context.Context, playerID string) error {
	_, err := c.Call(ctx, "players/cmd/next", playerArgs(playerID))
	return err
}

// Previous sends players/cmd/previous.
func (c *Client) Previous(ctx context.Context, playerID string) error {
	_, err := c.Call(ctx, "players/cmd/previous", playerArgs(playerID))
	return err
}

// Seek sends players/cmd/seek to the absolute position in seconds. MA has
// no relative-seek command; the daemon rejects relative seeks for both
// backends (observer-mode parity).
func (c *Client) Seek(ctx context.Context, playerID string, positionSeconds int) error {
	_, err := c.Call(ctx, "players/cmd/seek", map[string]any{
		"player_id": playerID,
		"position":  positionSeconds,
	})
	return err
}

// VolumeSet sends players/cmd/volume_set with level 0..100.
func (c *Client) VolumeSet(ctx context.Context, playerID string, level int) error {
	_, err := c.Call(ctx, "players/cmd/volume_set", map[string]any{
		"player_id":    playerID,
		"volume_level": level,
	})
	return err
}

// VolumeUp sends players/cmd/volume_up (one step on the player's own scale).
func (c *Client) VolumeUp(ctx context.Context, playerID string) error {
	_, err := c.Call(ctx, "players/cmd/volume_up", playerArgs(playerID))
	return err
}

// VolumeDown sends players/cmd/volume_down.
func (c *Client) VolumeDown(ctx context.Context, playerID string) error {
	_, err := c.Call(ctx, "players/cmd/volume_down", playerArgs(playerID))
	return err
}

// VolumeMute sends players/cmd/volume_mute with the explicit muted flag
// (the 2.10.7 schema makes "muted" a required boolean).
func (c *Client) VolumeMute(ctx context.Context, playerID string, muted bool) error {
	_, err := c.Call(ctx, "players/cmd/volume_mute", map[string]any{
		"player_id": playerID,
		"muted":     muted,
	})
	return err
}

// Shuffle enables/disables shuffle on the player's own queue via
// player_queues/shuffle. The command takes a queue_id - in MA a player's
// default queue carries the same id as the player itself (verified against
// the live schema: {queue_id, shuffle_enabled}, no player_ids form).
func (c *Client) Shuffle(ctx context.Context, playerID string, enabled bool) error {
	_, err := c.Call(ctx, "player_queues/shuffle", map[string]any{
		"queue_id":        playerID,
		"shuffle_enabled": enabled,
	})
	return err
}

// Repeat sends players/cmd/repeat with the given RepeatMode.
func (c *Client) Repeat(ctx context.Context, playerID string, mode string) error {
	_, err := c.Call(ctx, "players/cmd/repeat", map[string]any{
		"player_id":   playerID,
		"repeat_mode": mode,
	})
	return err
}
