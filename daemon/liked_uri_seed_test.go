package daemon

import "testing"

// issue #84: the CI-baked liked-songs playlist URI (Config.LikedPlaylistURI —
// injected into go-librespot-config.yml at firmware image-build time from the
// mira-firmware repo secret LIKED_PLAYLIST_URI, see config.yml) seeds the
// persisted state when it carries none, and never overwrites a value resolved
// elsewhere.

func TestSeedBakedLikedPlaylistURI_FillsEmptyState(t *testing.T) {
	t.Parallel()
	p, _ := newFix7Player(t)
	p.app.cfg = &Config{LikedPlaylistURI: "spotify:playlist:baked"}

	p.seedBakedLikedPlaylistURI()

	if got := p.cachedLikedPlaylistURI(); got != "spotify:playlist:baked" {
		t.Errorf("seeded uri: got %q want the baked config value", got)
	}
}

func TestSeedBakedLikedPlaylistURI_EmptyConfigLeavesState(t *testing.T) {
	t.Parallel()
	p, _ := newFix7Player(t)
	p.app.cfg = &Config{}

	p.seedBakedLikedPlaylistURI()

	if got := p.cachedLikedPlaylistURI(); got != "" {
		t.Errorf("uri: got %q want empty (no baked value)", got)
	}
}

func TestSeedBakedLikedPlaylistURI_NeverOverwritesResolved(t *testing.T) {
	t.Parallel()
	p, _ := newFix7Player(t)
	p.app.state.LikedPlaylistURI = "spotify:playlist:resolved-elsewhere"
	p.app.cfg = &Config{LikedPlaylistURI: "spotify:playlist:baked"}

	p.seedBakedLikedPlaylistURI()

	if got := p.cachedLikedPlaylistURI(); got != "spotify:playlist:resolved-elsewhere" {
		t.Errorf("uri: got %q want the already-resolved value (never overwritten)", got)
	}
}
