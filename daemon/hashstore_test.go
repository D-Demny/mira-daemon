package daemon

import (
	"strings"
	"testing"
)

func hx(c string) string { return strings.Repeat(c, 64) }

func TestHashStoreFallback(t *testing.T) {
	s := newHashStore()
	if got := s.hash("searchDesktop"); got != pfSearchDesktopHash {
		t.Fatalf("empty store: got %q want default %q", got, pfSearchDesktopHash)
	}
	a := hx("a")
	s.adopt(map[string]string{"searchDesktop": a})
	if got := s.hash("searchDesktop"); got != a {
		t.Fatalf("current: got %q want %q", got, a)
	}
	s2 := newHashStore()
	s2.seed(nil, map[string]string{"searchDesktop": a})
	if got := s2.hash("searchDesktop"); got != a {
		t.Fatalf("previous fallback: got %q want %q", got, a)
	}
	if got := s.hash("nonsense"); got != "" {
		t.Fatalf("unknown op: got %q want \"\"", got)
	}
}

func TestHashStoreNilSafe(t *testing.T) {
	var s *hashStore
	if got := s.hash("searchDesktop"); got != pfSearchDesktopHash {
		t.Fatalf("nil store: got %q want default", got)
	}
}

func TestHashStoreAdoptRotation(t *testing.T) {
	s := newHashStore()
	a, b := hx("a"), hx("b")

	if n := s.adopt(map[string]string{"searchDesktop": a}); n != 1 {
		t.Fatalf("first adopt changed=%d want 1", n)
	}
	if n := s.adopt(map[string]string{"searchDesktop": b}); n != 1 {
		t.Fatalf("second adopt changed=%d want 1", n)
	}
	cur, prev := s.snapshot()
	if cur["searchDesktop"] != b {
		t.Fatalf("current=%q want %q", cur["searchDesktop"], b)
	}
	if prev["searchDesktop"] != a {
		t.Fatalf("previous=%q want %q (the refreshed backup)", prev["searchDesktop"], a)
	}
	if n := s.adopt(map[string]string{"searchDesktop": b}); n != 0 {
		t.Fatalf("unchanged adopt changed=%d want 0", n)
	}
	if n := s.adopt(map[string]string{"searchDesktop": "deadbeef"}); n != 0 {
		t.Fatalf("invalid adopt changed=%d want 0", n)
	}
	if s.hash("searchDesktop") != b {
		t.Fatalf("invalid adopt clobbered current to %q", s.hash("searchDesktop"))
	}
}

func TestHashStoreSeedValidates(t *testing.T) {
	s := newHashStore()
	good := hx("c")
	s.seed(map[string]string{"searchDesktop": "not-hex", "addToLibrary": good}, nil)
	if got := s.hash("searchDesktop"); got != pfSearchDesktopHash {
		t.Fatalf("junk seed should drop -> default, got %q", got)
	}
	if got := s.hash("addToLibrary"); got != good {
		t.Fatalf("valid seed: got %q want %q", got, good)
	}
}

func TestHashStoreHasAllTargets(t *testing.T) {
	s := newHashStore()
	if s.hasAllTargets() {
		t.Fatal("empty store should not have all targets")
	}
	full := map[string]string{}
	for op := range opHashDefaults {
		full[op] = hx("a")
	}
	s.adopt(full)
	if !s.hasAllTargets() {
		t.Fatal("after adopting all ops, hasAllTargets should be true")
	}
}
