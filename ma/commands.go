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

// QueueOptionReplace is the play_media option the daemon always uses
// (issue #138): a new context replaces the current queue and plays from its
// first item — the same semantics Spotify Connect's context playback has.
const QueueOptionReplace = "replace"

// PlayMedia sends player_queues/play_media: play one media URI on the queue
// that carries the given id (for non-grouped players == the player id). MA
// 2.10.7 has no players/cmd/play_url — this is THE playback command for items.
// startItem, when set, must be an exact item ID or URI inside a playlist/
// album source: the source then starts at that item and queues the rest after
// it (verified live against 2.10.7's media_resolver exact-match path).
func (c *Client) PlayMedia(ctx context.Context, queueID, mediaURI, startItem string) error {
	args := map[string]any{
		"queue_id": queueID,
		"media":    []string{mediaURI},
		"option":   QueueOptionReplace,
	}
	if startItem != "" {
		args["start_item"] = startItem
	}
	_, err := c.Call(ctx, "player_queues/play_media", args)
	return err
}

// PlayIndex sends player_queues/play_index: start the CURRENT queue at the
// given 0-based item position without modifying it — the in-queue skip used
// when a "Läuft gerade" upcoming-card confirmation maps onto an observed
// queue entry. The 2.10.7 schema for index is int | str: a JSON STRING is
// resolved as a queue_item_id, and item ids are uuids — numeric position
// strings never match, so they 400 with "Item N not found in queue". The
// position MUST therefore be sent as a JSON number (verified against the
// live schema; issue #138).
func (c *Client) PlayIndex(ctx context.Context, queueID string, index int) error {
	_, err := c.Call(ctx, "player_queues/play_index", map[string]any{
		"queue_id": queueID,
		"index":    index,
	})
	return err
}

// Resume sends player_queues/resume: continue playback on the queue at its
// saved position. Verified against 2.10.7's controller.resume: a hard stop
// parks corrected_elapsed_time in queue.resume_pos and KEEPS current_item,
// so resume after stop continues that item where it stopped (live-verified:
// web queue idle with items=8, ended=false, resume_pos=211, current_item set);
// an ended queue restarts at item 0; a queue without a current item starts at
// the current index or first item. QueueEmpty (4xx) when the queue has no
// items - callers fall back to a replay in that case (issue #138 round 3,
// defect A').
func (c *Client) Resume(ctx context.Context, queueID string) error {
	_, err := c.Call(ctx, "player_queues/resume", map[string]any{"queue_id": queueID})
	return err
}

// TransferQueue sends player_queues/transfer: move the ENTIRE source queue to
// the target - all items, the sources, repeat/shuffle/crossfade/autoplay
// state and the current item with its resume position (issue #138 round 3,
// defect W). Verified against 2.10.7's controller.transfer_queue: it stops
// and clears the source, loads all items into the target and resumes there at
// the saved position only when the source was PLAYING (auto_play default); a
// grouped/synced TARGET is ungrouped first - the hand-rolled
// play_media+start_item replay could not do any of that and died with 500s on
// a WiiM group. Callers keep their own idle/no-media no-op gate.
func (c *Client) TransferQueue(ctx context.Context, sourceQueueID, targetQueueID string) error {
	_, err := c.Call(ctx, "player_queues/transfer", map[string]any{
		"source_queue_id": sourceQueueID,
		"target_queue_id": targetQueueID,
	})
	return err
}

// GetQueue calls player_queues/get for one queue id.
func (c *Client) GetQueue(ctx context.Context, queueID string) (*PlayerQueue, error) {
	raw, err := c.Call(ctx, "player_queues/get", map[string]any{"queue_id": queueID})
	if err != nil {
		return nil, err
	}
	var q PlayerQueue
	if err := json.Unmarshal(raw, &q); err != nil {
		return nil, fmt.Errorf("ma: decoding player_queues/get result for %s: %w", queueID, err)
	}
	return &q, nil
}

// QueueItems calls player_queues/items: the ACTUAL queue contents - at most
// limit items starting at 0-based offset. Unlike player_queues/get (count +
// snapshots only), this is the endpoint carrying item entries, and it is the
// same enumeration MA's own webui renders as the queue view. It is the ONLY
// correct source of "what plays next" for autoplay/re-queued queues: a queue
// can diverge from its declared source (live evidence on 2.10.7: a 27-item
// autoplay queue whose single source playlist holds 3 different tracks), so
// no source-based reconstruction can match it (issue #138, defect C).
// Item index fields in the response are RELATIVE to the requested offset -
// callers must add the offset back for absolute positions.
func (c *Client) QueueItems(ctx context.Context, queueID string, limit, offset int) ([]QueueItem, error) {
	raw, err := c.Call(ctx, "player_queues/items", map[string]any{
		"queue_id": queueID,
		"limit":    limit,
		"offset":   offset,
	})
	if err != nil {
		return nil, err
	}
	var items []QueueItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("ma: decoding player_queues/items result for %s: %w", queueID, err)
	}
	return items, nil
}

// Browse calls music/browse at the given path. Path "root" (or "") lists the
// provider folders; "<provider instance>://playlists" lists that provider's
// playlists. The result is a heterogeneous list — folders and media items in
// one struct (see BrowseItem).
func (c *Client) Browse(ctx context.Context, path string) ([]BrowseItem, error) {
	raw, err := c.Call(ctx, "music/browse", map[string]any{"path": path})
	if err != nil {
		return nil, err
	}
	var items []BrowseItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("ma: decoding music/browse result for %q: %w", path, err)
	}
	return items, nil
}

// PlaylistTracks calls music/playlists/playlist_tracks for one playlist.
// Playlists always surface as LIBRARY items (provider "library") regardless
// of the provider they were synced from — MA resolves the library row to the
// underlying provider internally (verified live against 2.10.7 for both
// built-in and Spotify-synced playlists).
func (c *Client) PlaylistTracks(ctx context.Context, itemID string) ([]TrackItem, error) {
	raw, err := c.Call(ctx, "music/playlists/playlist_tracks", map[string]any{
		"item_id":                        itemID,
		"provider_instance_id_or_domain": "library",
	})
	if err != nil {
		return nil, err
	}
	var tracks []TrackItem
	if err := json.Unmarshal(raw, &tracks); err != nil {
		return nil, fmt.Errorf("ma: decoding playlist_tracks result for %s: %w", itemID, err)
	}
	return tracks, nil
}

// AlbumTracks calls music/albums/album_tracks for one album. Album sources
// of a queue surface with their PROVIDER id + instance (not the library row),
// hence the explicit provider argument (issue #138). The tracks come back in
// album order (track number / disc).
func (c *Client) AlbumTracks(ctx context.Context, itemID, provider string) ([]TrackItem, error) {
	raw, err := c.Call(ctx, "music/albums/album_tracks", map[string]any{
		"item_id":                        itemID,
		"provider_instance_id_or_domain": provider,
	})
	if err != nil {
		return nil, err
	}
	var tracks []TrackItem
	if err := json.Unmarshal(raw, &tracks); err != nil {
		return nil, fmt.Errorf("ma: decoding album_tracks result for %s/%s: %w", provider, itemID, err)
	}
	return tracks, nil
}

// RecentlyPlayed calls music/recently_played_items (the play log). mediaTypes
// restricts the log rows (empty = MA default: all types); fullyPlayedOnly
// mirrors MA's API DEFAULT (true): with it set, an entry only counts once
// the item played to the END - a skipped item never lands in the list at
// all. Pass false explicitly for the "Zuletzt" pane and the per-speaker
// last-song lookup so partially played items surface immediately (issue
// #138 round 4, defect B'). The client still re-filters to track/playlist rows
// with a non-empty uri (the log also contains album/artist rows).
func (c *Client) RecentlyPlayed(ctx context.Context, limit int, queueID string, mediaTypes []string, fullyPlayedOnly bool) ([]TrackItem, error) {
	args := map[string]any{
		"limit":             limit,
		"fully_played_only": fullyPlayedOnly,
	}
	if len(mediaTypes) > 0 {
		args["media_types"] = mediaTypes
	}
	if queueID != "" {
		args["queue_id"] = queueID
	}
	raw, err := c.Call(ctx, "music/recently_played_items", args)
	if err != nil {
		return nil, err
	}
	var items []TrackItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("ma: decoding recently_played_items result: %w", err)
	}
	tracks := make([]TrackItem, 0, len(items))
	for _, it := range items {
		if (it.MediaType == "track" || it.MediaType == "playlist") && it.URI != "" {
			tracks = append(tracks, it)
		}
	}
	return tracks, nil
}
