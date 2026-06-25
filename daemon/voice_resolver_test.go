package daemon

import (
	"context"
	"testing"
)

// deterministic stand in for espeak so the logic is testable on my pc
var fakeIPA = map[string]string{
	// artists
	"kanye west":     "kaniwɛst",
	"connie west":    "kaniwɛst",
	"connie list":    "kanilɪst",
	"kenny west":     "kɛniwɛst",
	"drake":          "dɹeɪk",
	"the weeknd":     "ðəwiːkɛnd",
	"weeknd":         "wiːkɛnd",
	"taylor swift":   "teɪlɚswɪft",
	"kendrick lamar": "kɛndɹɪkləmɑːɹ",
	// tracks
	"heartless":       "hɑːɹtləs",
	"vultures":        "vʌltʃɚz",
	"burn":            "bɝn",
	"blinding lights": "blaɪndɪŋlaɪts",
	"stronger":        "stɹɔŋɚ",
	"calcium":         "kælsiəm",
	"ecco2k":          "ɛkoʊtuːkeɪ",
	// playlists/albums
	"workout":     "wɝkaʊt",
	"chill vibes": "tʃɪlvaɪbz",
	"chill vibe":  "tʃɪlvaɪb",
	"graduation":  "ɡɹædʒueɪʃən",
	// out of library
	"midnight protocol": "mɪdnaɪtpɹoʊtəkɔl",
	"zelda technical":   "zɛldətɛknɪkəl",
	// control verb mishear
	"go back": "ɡoʊbæk",
	"go bach": "ɡoʊbɑːk",
	"next":    "nɛkst",
	"necks":   "nɛks",
	"skip":    "skɪp",
	"skipped": "skɪpt",
}

func fakeG2P() *g2p {
	g := newG2P("", "", "", "")
	g.override = func(s string) string {
		if v, ok := fakeIPA[s]; ok {
			return v
		}
		return s
	}
	return g
}

func testIndex() *routedIndex {
	g := fakeG2P()
	ctx := context.Background()
	mkT := func(name, artist, uri string) indexEntry {
		return indexEntry{Name: name, Artist: artist, Uri: uri, Ipa: g.ipa(ctx, name), ArtistIpa: g.ipa(ctx, artist)}
	}
	mkN := func(name, uri string) indexEntry {
		return indexEntry{Name: name, Uri: uri, Ipa: g.ipa(ctx, name)}
	}
	mkA := func(name, artist, uri string) indexEntry {
		return indexEntry{Name: name, Artist: artist, Uri: uri, Ipa: g.ipa(ctx, name)}
	}
	return &routedIndex{
		Tracks: []indexEntry{
			mkT("Heartless", "Kanye West", "spotify:track:heartless"),
			mkT("Vultures", "Kanye West", "spotify:track:vultures"),
			mkT("Burn", "Kanye West", "spotify:track:burn"),
			mkT("Blinding Lights", "The Weeknd", "spotify:track:blinding"),
			mkT("Stronger", "Kanye West", "spotify:track:stronger"),
			mkT("Calcium", "Ecco2k", "spotify:track:calcium"),
		},
		Artists: []indexEntry{
			mkN("Kanye West", "spotify:artist:kanye"),
			mkN("Drake", "spotify:artist:drake"),
			mkN("The Weeknd", "spotify:artist:weeknd"),
			mkN("Taylor Swift", "spotify:artist:taylor"),
			mkN("Kendrick Lamar", "spotify:artist:kendrick"),
		},
		Playlists: []indexEntry{
			mkN("Workout", "spotify:playlist:workout"),
			mkN("Chill Vibes", "spotify:playlist:chill"),
		},
		Albums: []indexEntry{
			mkA("Graduation", "Kanye West", "spotify:album:graduation"),
		},
		Liked: []indexEntry{
			{Name: "Heartless", Artist: "Kanye West", Uri: "spotify:track:heartless"},
			{Name: "Calcium", Artist: "Ecco2k", Uri: "spotify:track:calcium"},
		},
	}
}

func newTestResolver() *cascadeResolver {
	r := newCascadeResolver(fakeG2P(), 0)
	r.setIndex(testIndex())
	return r
}

func TestClassify(t *testing.T) {
	r := newTestResolver()
	ctx := context.Background()
	cases := []struct {
		in   string
		kind string
	}{
		{"pause", kindControl},
		{"skip this song", kindControl},
		{"next song", kindControl},
		{"go back", kindControl},
		{"turn it up", kindControl},
		{"volume down", kindControl},
		{"like this song", kindBare},
		{"unlike this", kindBare},
		{"shuffle", kindBare},
		{"back in black", kindBare},
		{"next to you", kindBare},
		{"shuffle the deck", kindBare},
		{"play my liked songs", kindPlayLiked},
		{"surprise me", kindRandom},
		{"play a random song", kindRandom},
		{"queue heartless by kanye west", kindQueue},
		{"add calcium to queue", kindQueue},
		{"play heartless by kanye west", kindTrack},
		{"play drake radio", kindArtist},
		{"play taylor swift discography", kindArtist},
		{"play my workout playlist", kindPlaylist},
		{"play the album graduation", kindAlbum},
		{"play blinding lights", kindBare},
	}
	for _, c := range cases {
		got := r.classify(ctx, c.in)
		if got.kind != c.kind {
			t.Errorf("classify(%q) kind = %q, want %q", c.in, got.kind, c.kind)
		}
	}
}

func TestResolveControlVerbs(t *testing.T) {
	r := newTestResolver()
	ctx := context.Background()
	control := map[string]string{
		"stop":             "pause",
		"pause please":     "pause",
		"resume":           "resume",
		"next song":        "next",
		"skip it":          "next",
		"go back":          "prev",
		"previous":         "prev",
		"turn it up":       "volup",
		"volume up":        "volup",
		"turn it down":     "voldown",
		"volume down":      "voldown",
		"paused":           "pause",
		"go bach":          "prev",
		"necks":            "next",
		"skipped this one": "next",
	}
	for in, action := range control {
		d := r.resolve(ctx, []string{in})
		if d.Tier != "control" || d.Action != action {
			t.Errorf("resolve(%q) = tier=%s action=%s, want control/%s", in, d.Tier, d.Action, action)
		}
	}

	for _, in := range []string{"back in black", "next to you", "shuffle the deck"} {
		d := r.resolve(ctx, []string{in})
		if d.Tier == "control" {
			t.Errorf("resolve(%q) fired control %q, want a content tier", in, d.Action)
		}
	}
}

func TestResolveControlDeferredToLocal(t *testing.T) {
	r := newTestResolver()
	hyps := []string{"back", "play heartless by kanye west"}
	d := r.resolve(context.Background(), hyps)
	if d.Tier != "local" || d.Uri != "spotify:track:heartless" {
		t.Fatalf("control-vs-local = %+v, want local Heartless (control deferred)", d)
	}
}

func TestResolveControl(t *testing.T) {
	r := newTestResolver()
	for in, action := range map[string]string{
		"pause": "pause", "skip this song": "next", "next": "next",
		"back": "prev", "previous": "prev", "resume": "resume",
	} {
		d := r.resolve(context.Background(), []string{in})
		if d.Tier != "control" || d.Action != action {
			t.Errorf("resolve(%q) = %+v, want control/%s", in, d, action)
		}
	}
}

func TestResolveTrackAnchoredJoint(t *testing.T) {
	r := newTestResolver()
	cases := []struct {
		hyp      string
		wantUri  string
		wantName string
	}{
		{"Play Heartless by Connie West", "spotify:track:heartless", "Heartless"},
		{"Play Burn by Connie West", "spotify:track:burn", "Burn"},
	}
	for _, c := range cases {
		d := r.resolve(context.Background(), []string{c.hyp})
		if d.Tier != "local" || d.Uri != c.wantUri {
			t.Errorf("resolve(%q) = tier=%s uri=%s (%s), want local %s (%s)", c.hyp, d.Tier, d.Uri, d.Name, c.wantUri, c.wantName)
		}
		if d.Artist != "Kanye West" {
			t.Errorf("resolve(%q) artist = %q, want Kanye West", c.hyp, d.Artist)
		}
	}
}

func TestResolveMultiHypothesis(t *testing.T) {
	r := newTestResolver()
	hyps := []string{
		"Please vote for his iconic list",
		"Play of Vultures by Connie List",
		"Please vote by Connie List",
	}
	d := r.resolve(context.Background(), hyps)
	if d.Tier != "local" || d.Uri != "spotify:track:vultures" {
		t.Fatalf("multi-hyp resolve = %+v, want local Vultures", d)
	}
}

func TestResolveBareTrackPrefix(t *testing.T) {
	r := newTestResolver()
	d := r.resolve(context.Background(), []string{"play calcium back on the key"})
	if d.Tier != "local" || d.Uri != "spotify:track:calcium" {
		t.Fatalf("bare-prefix resolve = tier=%s uri=%s (%s, score=%.3f), want local Calcium",
			d.Tier, d.Uri, d.Name, d.Score)
	}
	d2 := r.resolve(context.Background(), []string{"play blinding lights"})
	if d2.Tier != "local" || d2.Uri != "spotify:track:blinding" {
		t.Fatalf("bare full-match regressed = %+v, want local Blinding Lights", d2)
	}
}

func TestResolveArtistPlaylistAlbum(t *testing.T) {
	r := newTestResolver()
	cases := []struct {
		hyp     string
		kind    string
		wantUri string
	}{
		{"play drake radio", kindArtist, "spotify:artist:drake"},
		{"play my workout playlist", kindPlaylist, "spotify:playlist:workout"},
		{"play chill vibe playlist", kindPlaylist, "spotify:playlist:chill"},
		{"play the album graduation", kindAlbum, "spotify:album:graduation"},
		{"play blinding lights", kindTrack, "spotify:track:blinding"},
	}
	for _, c := range cases {
		d := r.resolve(context.Background(), []string{c.hyp})
		if d.Tier != "local" || d.Kind != c.kind || d.Uri != c.wantUri {
			t.Errorf("resolve(%q) = tier=%s kind=%s uri=%s, want local %s %s", c.hyp, d.Tier, d.Kind, d.Uri, c.kind, c.wantUri)
		}
	}
}

func TestResolveOutOfLibraryFallback(t *testing.T) {
	r := newTestResolver()
	d := r.resolve(context.Background(), []string{"play midnight protocol by zelda technical"})
	if d.Tier != "search" {
		t.Fatalf("out-of-library resolve = %+v, want search fallback", d)
	}
	if d.Query == "" {
		t.Fatalf("search fallback carried no query")
	}
}

func TestResolveEmptyIndexFallsThrough(t *testing.T) {
	r := newCascadeResolver(fakeG2P(), 0)
	d := r.resolve(context.Background(), []string{"play heartless by kanye west"})
	if d.Tier != "search" {
		t.Fatalf("empty-index resolve = %+v, want search fallback", d)
	}
	dc := r.resolve(context.Background(), []string{"pause"})
	if dc.Tier != "control" || dc.Action != "pause" {
		t.Fatalf("control without index = %+v, want control/pause", dc)
	}
}

func TestResolveQueue(t *testing.T) {
	r := newTestResolver()
	ctx := context.Background()
	cases := []struct {
		hyp     string
		wantUri string
	}{
		{"queue heartless by kanye west", "spotify:track:heartless"},
		{"queue calcium", "spotify:track:calcium"},
		{"add calcium to queue", "spotify:track:calcium"},
	}
	for _, c := range cases {
		d := r.resolve(ctx, []string{c.hyp})
		if d.Tier != "queue" || d.Uri != c.wantUri {
			t.Errorf("resolve(%q) = tier=%s uri=%s, want queue %s", c.hyp, d.Tier, d.Uri, c.wantUri)
		}
	}
	// out of library queue shouldnt search (not reliable)
	d := r.resolve(ctx, []string{"queue midnight protocol by zelda technical"})
	if d.Tier != "abstain" || d.Kind != kindQueue {
		t.Fatalf("out-of-library queue = %+v, want abstain/queue", d)
	}
}

func TestResolvePlayLiked(t *testing.T) {
	r := newTestResolver()
	ctx := context.Background()
	for _, in := range []string{"play my liked songs", "play my liked", "liked songs"} {
		d := r.resolve(ctx, []string{in})
		if d.Tier != "local" || d.Uri != likedCollectionUri {
			t.Errorf("resolve(%q) = %+v, want local %s", in, d, likedCollectionUri)
		}
	}
}

func TestResolveRandom(t *testing.T) {
	r := newTestResolver()
	ctx := context.Background()
	liked := map[string]bool{"spotify:track:heartless": true, "spotify:track:calcium": true}
	for _, in := range []string{"surprise me", "play a random song", "random"} {
		d := r.resolve(ctx, []string{in})
		if d.Tier != "local" || !liked[d.Uri] {
			t.Errorf("resolve(%q) = %+v, want local from Liked Songs", in, d)
		}
	}
	r2 := newCascadeResolver(fakeG2P(), 0)
	r2.setIndex(&routedIndex{Tracks: testIndex().Tracks})
	d := r2.resolve(ctx, []string{"surprise me"})
	if d.Tier != "abstain" || d.Kind != kindRandom {
		t.Fatalf("random without Liked = %+v, want abstain/random", d)
	}
}

func TestResolveFix3LeadVerb(t *testing.T) {
	r := newTestResolver()
	ctx := context.Background()
	d := r.resolve(ctx, []string{"clay heartless"})
	if d.Tier != "local" || d.Uri != "spotify:track:heartless" {
		t.Fatalf("clay-heartless = %+v, want local Heartless", d)
	}
	d2 := r.resolve(ctx, []string{"clay my workout"})
	if d2.Tier != "local" || d2.Kind != kindPlaylist || d2.Uri != "spotify:playlist:workout" {
		t.Fatalf("clay-my-workout = %+v, want local playlist Workout", d2)
	}
	if got := r.classify(ctx, "clay"); got.kind != kindBare || got.slots["name"] != "clay" {
		t.Fatalf("lone 'clay' = %+v, want bare name=clay", got)
	}
}

func TestResolveFix4Suffix(t *testing.T) {
	r := newTestResolver()
	d := r.resolve(context.Background(), []string{"play heartless songs"})
	if d.Tier != "local" || d.Kind != kindTrack || d.Uri != "spotify:track:heartless" {
		t.Fatalf("heartless-songs = %+v, want local track Heartless", d)
	}
}

func TestFloorFor(t *testing.T) {
	r := newTestResolver()
	cases := []struct {
		m    *localCandidate
		want float64
	}{
		{&localCandidate{kind: kindTrack, anchored: true}, cascadeTrackAnchoredFloor},
		{&localCandidate{kind: kindTrack, bare: true}, bareFloor},
		{&localCandidate{kind: kindArtist}, r.accept},
		{&localCandidate{kind: kindTrack}, r.accept},
	}
	for _, c := range cases {
		if f := r.floorFor(c.m); f != c.want {
			t.Errorf("floorFor(%+v) = %v, want %v", c.m, f, c.want)
		}
	}
}

func TestResolveBareFloorRejectsWeakBand(t *testing.T) {
	g := fakeG2P()
	g.override = func(s string) string {
		switch s {
		case "midband":
			return "aaaaaa"
		case "midbend":
			return "aaaabb"
		}
		return s
	}
	ctx := context.Background()
	r := newCascadeResolver(g, 0)
	r.setIndex(&routedIndex{Tracks: []indexEntry{{Name: "Midband", Uri: "spotify:track:mid", Ipa: g.ipa(ctx, "midband")}}})

	if d := r.resolve(ctx, []string{"play midbend"}); d.Tier != "search" {
		t.Fatalf("bare 0.333 = %+v, want search (rejected by the stricter bareFloor)", d)
	}
	if d := r.resolve(ctx, []string{"play midband"}); d.Tier != "local" || d.Uri != "spotify:track:mid" {
		t.Fatalf("clean bare 0.0 = %+v, want local", d)
	}
}

func TestNormDistAndLev(t *testing.T) {
	if d := levRunes([]rune("abc"), []rune("abc")); d != 0 {
		t.Errorf("identical lev = %d, want 0", d)
	}
	if d := levRunes([]rune("abc"), []rune("abd")); d != 1 {
		t.Errorf("one-sub lev = %d, want 1", d)
	}
	if nd := normDist([]rune("kaniwɛst"), []rune("kaniwɛst")); nd != 0 {
		t.Errorf("identical normDist = %f, want 0", nd)
	}
	if nd := normDist([]rune(""), []rune("")); nd != 0 {
		t.Errorf("empty normDist = %f, want 0", nd)
	}
}

func TestCleanIPA(t *testing.T) {
	if got := cleanIPA("plˈeɪ hˈɑːɹtləs"); got != "pleɪhɑːɹtləs" {
		t.Errorf("cleanIPA = %q, want pleɪhɑːɹtləs", got)
	}
}

func TestRerankSearch(t *testing.T) {
	r := newTestResolver()
	ctx := context.Background()
	results := []searchTrackResult{
		{Uri: "spotify:track:backup", Name: "Backup Plan", Artist: "Bailey Zimmerman"},
		{Uri: "spotify:track:foronce", Name: "For Once", Artist: "BaileyRP"},
	}
	pick, score, ok := r.rerankSearch(ctx, "for once", "bailey rp", results)
	if !ok {
		t.Fatalf("expected a pick, got abstain (score=%.3f)", score)
	}
	if pick.Uri != "spotify:track:foronce" {
		t.Fatalf("re-rank picked %q/%q, want For Once/BaileyRP (score=%.3f)", pick.Name, pick.Artist, score)
	}
	if _, _, ok := r.rerankSearch(ctx, "zzzzqwxv", "plplpl", results); ok {
		t.Fatalf("expected ABSTAIN on gibberish, got a pick")
	}
	if _, _, ok := r.rerankSearch(ctx, "", "", results); ok {
		t.Fatalf("empty track slot should abstain")
	}
	if _, _, ok := r.rerankSearch(ctx, "for once", "", nil); ok {
		t.Fatalf("no results should abstain")
	}
}

func TestSearchTermsLight(t *testing.T) {
	cases := []struct{ in, q, tr, ar string }{
		{"play for once by bailey rp", "for once bailey rp", "for once", "bailey rp"},
		{"play viva la vida by coldplay", "viva la vida coldplay", "viva la vida", "coldplay"},
		{"clay heartless", "heartless", "heartless", ""},
		{"the less i know the better", "the less i know the better", "the less i know the better", ""},
	}
	for _, c := range cases {
		q, tr, ar := searchTermsLight(c.in)
		if q != c.q || tr != c.tr || ar != c.ar {
			t.Errorf("searchTermsLight(%q) = (%q,%q,%q), want (%q,%q,%q)", c.in, q, tr, ar, c.q, c.tr, c.ar)
		}
	}
}
