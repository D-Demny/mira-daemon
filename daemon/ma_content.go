// Shared Music Assistant content fetch + mapping (issue #138).
//
// These are free functions on top of ma.Client so that the SAME request
// shapes can be served from two places:
//   - the session path: AppPlayer methods in player_ma.go (post-session,
//     with the warmup cache around the playlist browse);
//   - the pre-session path: ConcreteApiServer.serveMaPreSession in
//     api_server.go. After a flash the daemon sits in Spotify device-flow
//     pairing for minutes and Run() - the only consumer of the request
//     channel - starts only after it completes, so the read-only /ma/*
//     content endpoints are answered directly from the persisted settings
//     (which survive the flash) with a throwaway client (defect B').
package daemon

import (
	"context"
	"fmt"
	"strings"

	librespot "github.com/devgianlu/go-librespot"

	"github.com/devgianlu/go-librespot/ma"
)

// maBrowsePlaylists runs the root + per-provider playlist browse on the given
// client. A provider without a playlists collection answers its browse with a
// 500 - that folder is skipped, not an error (verified live against 2.10.7).
// Only when EVERY provider folder fails does it return an error; an empty
// result with at least one successful browse is a valid "no playlists".
func maBrowsePlaylists(ctx context.Context, log librespot.Logger, client *ma.Client) ([]MaPlaylist, error) {
	root, err := client.Browse(ctx, "root")
	if err != nil {
		log.Warnf("ma: music/browse root failed: %v", err)
		return nil, fmt.Errorf("music assistant playlist browse failed: %w", err)
	}
	playlists := []MaPlaylist{}
	folders, failedFolders := 0, 0
	for _, folder := range root {
		if !strings.HasSuffix(folder.Path, "://") {
			continue // not a provider folder (breadcrumbs etc.)
		}
		folders++
		items, err := client.Browse(ctx, folder.Path+"playlists")
		if err != nil {
			log.Debugf("ma: music/browse %splaylists failed: %v", folder.Path, err)
			failedFolders++
			continue
		}
		for _, it := range items {
			if it.MediaType != "playlist" || it.URI == "" {
				continue // back-breadcrumb folders and non-playlists
			}
			pl := MaPlaylist{ItemID: it.ItemID, Name: it.Name, URI: it.URI}
			pl.ImageURL = maPlaylistImage(it)
			playlists = append(playlists, pl)
		}
	}
	if folders > 0 && failedFolders == folders {
		return nil, fmt.Errorf("music assistant playlist browse failed for all %d providers", folders)
	}
	return playlists, nil
}

// maMapDevices maps MA's players/all response to the /ma/devices contract
// shape, adding one synthetic native-group entry per multi-member master
// (issue #138: selecting it resolves to the master whose WiiM firmware
// mirrors playback to the slaves natively - deliberately NOT an MA virtual
// group; see maResolvePlayerID).
func maMapDevices(players []ma.Player) []MaDevice {
	devs := make([]MaDevice, 0, len(players))
	for _, pl := range players {
		devs = append(devs, MaDevice{Id: pl.PlayerID, Name: pl.Name, Available: pl.Available})
	}
	for _, pl := range players {
		if len(pl.GroupMembers) > 1 {
			devs = append(devs, MaDevice{Id: maGroupIDPrefix + pl.PlayerID, Name: pl.Name + " (Gruppe)", Available: pl.Available})
		}
	}
	return devs
}

// maMapPlaylistItems maps music/playlists/playlist_tracks entries to the UI
// track-card shape in playlist order (Position is the absolute index).
func maMapPlaylistItems(items []ma.TrackItem) []MaTrack {
	tracks := []MaTrack{}
	for i := range items {
		it := &items[i]
		if it.URI == "" {
			continue
		}
		names := make([]string, 0, len(it.Artists))
		for _, a := range it.Artists {
			if a.Name != "" {
				names = append(names, a.Name)
			}
		}
		t := MaTrack{ID: it.ItemID, Name: it.Name, URI: it.URI, Album: it.Album.Name(), Artists: names, Position: it.Position}
		if it.Duration > 0 {
			t.DurationMs = int64(it.Duration) * 1000
		}
		t.ImageURL = maImageURL(it.Cover())
		tracks = append(tracks, t)
	}
	return tracks
}

// maMapRecentItems maps music/recently_played_items entries to the UI
// track-card shape. The play-log mapping is lightweight - title + artwork
// only, no artists (MA 2.10.7 limitation), so the UI renders title-only
// cards for the "Zuletzt" pane in MA mode.
func maMapRecentItems(items []ma.TrackItem) []MaTrack {
	tracks := []MaTrack{}
	for i := range items {
		it := &items[i]
		t := MaTrack{ID: it.ItemID, Name: it.Name, URI: it.URI, Artists: []string{}}
		t.ImageURL = maImageURL(it.Image)
		tracks = append(tracks, t)
	}
	return tracks
}
