package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestExtractTargetOps(t *testing.T) {
	search, add, top := hx("e"), hx("6"), hx("4")
	js := []byte(`x=new D.l("searchDesktop","query","` + search + `",null);` +
		`y=new(r(41935)).l("addToLibrary","mutation","` + add + `",null);` +
		`z=new Q.l("someOtherOp","query","` + hx("9") + `",null);` +
		`w=new Q.l("userTopContent","query","` + top + `",null)`)
	got := extractTargetOps(js)
	want := map[string]string{"searchDesktop": search, "addToLibrary": add, "userTopContent": top}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extractTargetOps:\n got  %v\n want %v", got, want)
	}
}

func TestExtractTargetOpsGibberish(t *testing.T) {
	for _, in := range []string{
		"this is not javascript 123 \"searchDesktop\" lol",
		`new D.l("searchDesktop","query","tooshort",null)`,
		`new D.l("searchDesktop","query","` + hx("g") + `",null)`,
		"",
	} {
		if got := extractTargetOps([]byte(in)); len(got) != 0 {
			t.Fatalf("gibberish %q yielded %v, want empty", in, got)
		}
	}
}

func TestHaveAllTargets(t *testing.T) {
	full := map[string]string{}
	for op := range opHashDefaults {
		full[op] = hx("a")
	}
	if !haveAllTargets(full) {
		t.Fatal("full table should pass")
	}
	delete(full, "searchDesktop")
	if haveAllTargets(full) {
		t.Fatal("missing op should fail")
	}
	full["searchDesktop"] = "not-hex"
	if haveAllTargets(full) {
		t.Fatal("invalid hex should fail")
	}
}

func TestRouteChunkURLs(t *testing.T) {
	main := []byte(`(({4406:"xpui-routes-search",7969:"xpui-routes-profile",100:"xpui-routes-config"})[e]||e)+"."+` +
		`({4406:"db3b2d9e",7969:"1ffd9683",100:"aabbccdd",9958:"80d06e1a"})[e]+".js"`)
	urls := routeChunkURLs("https://cdn/build/web-player/", main)
	want := []string{
		"https://cdn/build/web-player/xpui-routes-search.db3b2d9e.js",
		"https://cdn/build/web-player/xpui-routes-profile.1ffd9683.js",
	}
	if !reflect.DeepEqual(urls, want) {
		t.Fatalf("routeChunkURLs:\n got  %v\n want %v", urls, want)
	}
}

func TestHashCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newHashStore()
	s.adopt(map[string]string{"searchDesktop": hx("a")})
	s.adopt(map[string]string{"searchDesktop": hx("b")})
	s.adopt(map[string]string{"libraryV3": hx("c")})

	if err := saveHashCacheFile(dir, "web-player.deadbeef.js", 12345, s); err != nil {
		t.Fatalf("save: %v", err)
	}
	s2 := newHashStore()
	bundle, fetchedAt := loadHashCacheFile(dir, s2)
	if bundle != "web-player.deadbeef.js" || fetchedAt != 12345 {
		t.Fatalf("meta: bundle=%q fetchedAt=%d", bundle, fetchedAt)
	}
	cur, prev := s2.snapshot()
	if cur["searchDesktop"] != hx("b") || prev["searchDesktop"] != hx("a") || cur["libraryV3"] != hx("c") {
		t.Fatalf("round-trip mismatch: cur=%v prev=%v", cur, prev)
	}
}

func TestHashCacheRejectsBadVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(hashCachePath(dir), []byte(`{"version":999,"current":{"searchDesktop":"`+hx("a")+`"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newHashStore()
	if bundle, _ := loadHashCacheFile(dir, s); bundle != "" {
		t.Fatal("bad version should be ignored")
	}
	if s.hash("searchDesktop") != pfSearchDesktopHash {
		t.Fatal("bad version should not seed the store")
	}
}

func TestScrapeViaHTTPTest(t *testing.T) {
	want := map[string]string{
		"searchDesktop":        hx("e"),
		"userTopContent":       hx("4"),
		"fetchLibraryTracks":   hx("1"),
		"libraryV3":            hx("2"),
		"fetchPlaylist":        hx("3"),
		"areEntitiesInLibrary": hx("5"),
		"addToLibrary":         hx("6"),
		"applyCurations":       hx("7"),
	}

	var fix struct{ home, main, search, profile []byte }
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			_, _ = w.Write(fix.home)
		case strings.HasSuffix(r.URL.Path, "/web-player.deadbeef.js"):
			_, _ = w.Write(fix.main)
		case strings.HasSuffix(r.URL.Path, "/xpui-routes-search.aaaaaaaa.js"):
			_, _ = w.Write(fix.search)
		case strings.HasSuffix(r.URL.Path, "/xpui-routes-profile.bbbbbbbb.js"):
			_, _ = w.Write(fix.profile)
		default:
			http.NotFound(w, r)
		}
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	base := ts.URL + "/cdn/build/web-player/"

	mainBody := `self.p="` + base + `";` +
		`a=new T.l("fetchLibraryTracks","query","` + want["fetchLibraryTracks"] + `",null);` +
		`b=new T.l("libraryV3","query","` + want["libraryV3"] + `",null);` +
		`c=new T.l("fetchPlaylist","query","` + want["fetchPlaylist"] + `",null);` +
		`d=new T.l("areEntitiesInLibrary","query","` + want["areEntitiesInLibrary"] + `",null);` +
		`e=new T.l("addToLibrary","mutation","` + want["addToLibrary"] + `",null);` +
		`f=new T.l("applyCurations","mutation","` + want["applyCurations"] + `",null);` +
		`u=(({4406:"xpui-routes-search",7969:"xpui-routes-profile"})[x]||x)+"."+({4406:"aaaaaaaa",7969:"bbbbbbbb"})[x]+".js"`
	fix.home = []byte(`<html><script src="` + base + `web-player.deadbeef.js"></script></html>`)
	fix.main = []byte(mainBody)
	fix.search = []byte(`q=new P.l("searchDesktop","query","` + want["searchDesktop"] + `",null)`)
	fix.profile = []byte(`q=new P.l("userTopContent","query","` + want["userTopContent"] + `",null)`)

	sc := &hashScraper{client: &http.Client{Timeout: 5 * time.Second}, log: nopLogger{}, home: ts.URL + "/"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	found, bundle, err := sc.scrape(ctx, "")
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if bundle != "web-player.deadbeef.js" {
		t.Fatalf("bundle=%q", bundle)
	}
	if !haveAllTargets(found) {
		t.Fatalf("incomplete: %v", found)
	}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("scrape result:\n got  %v\n want %v", found, want)
	}

	got2, b2, err := sc.scrape(ctx, "web-player.deadbeef.js")
	if err != nil || got2 != nil || b2 != "web-player.deadbeef.js" {
		t.Fatalf("unchanged short-circuit: got=%v bundle=%q err=%v", got2, b2, err)
	}
}

func TestHashScrapeLive(t *testing.T) {
	if os.Getenv("HASH_SCRAPE_LIVE") == "" {
		t.Skip("set HASH_SCRAPE_LIVE=1 to hit open.spotify.com")
	}
	sc := newHashScraper(nopLogger{})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	found, bundle, err := sc.scrape(ctx, "")
	if err != nil {
		t.Fatalf("live scrape: %v", err)
	}
	t.Logf("bundle = %s", bundle)
	if !haveAllTargets(found) {
		t.Fatalf("incomplete live scrape (%d/%d): %v", countHex(found), len(opHashDefaults), found)
	}
	for op := range opHashDefaults {
		drift := ""
		if found[op] != opHashDefaults[op] {
			drift = "   <<< DRIFTED vs hardcoded"
		}
		t.Logf("%-22s %s%s", op, found[op], drift)
		if !is64Hex(found[op]) {
			t.Errorf("%s not 64-hex: %q", op, found[op])
		}
	}
}
