package daemon

import "sync"

// seeds the hash store with the hashes
var opHashDefaults = map[string]string{
	"searchDesktop":        pfSearchDesktopHash,
	"fetchLibraryTracks":   pfFetchLibraryTracksHash,
	"libraryV3":            pfLibraryV3Hash,
	"fetchPlaylist":        pfFetchPlaylistHash,
	"userTopContent":       pfUserTopContentHash,
	"areEntitiesInLibrary": pfAreEntitiesInLibraryHash,
	"addToLibrary":         pfAddToLibraryHash,
	"applyCurations":       pfApplyCurationsHash,
	"recents":              pfRecentsHash,
}

// hashStore holds the live pathfinder hashes that are scraped
type hashStore struct {
	mu       sync.RWMutex
	current  map[string]string
	previous map[string]string
}

func newHashStore() *hashStore {
	return &hashStore{current: map[string]string{}, previous: map[string]string{}}
}

// returns the best hash we have for an operation
func (s *hashStore) hash(op string) string {
	if s == nil {
		return opHashDefaults[op]
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if h := s.current[op]; h != "" {
		return h
	}
	if h := s.previous[op]; h != "" {
		return h
	}
	return opHashDefaults[op]
}

func (s *hashStore) adopt(scraped map[string]string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := 0
	for op := range opHashDefaults {
		h := scraped[op]
		if !is64Hex(h) {
			continue
		}
		effective := s.current[op]
		if effective == "" {
			effective = s.previous[op]
		}
		if effective == "" {
			effective = opHashDefaults[op]
		}
		if effective == h {
			s.current[op] = h
			continue
		}
		if cur := s.current[op]; cur != "" {
			s.previous[op] = cur
		}
		s.current[op] = h
		changed++
	}
	return changed
}

func (s *hashStore) seed(current, previous map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for op := range opHashDefaults {
		if h := current[op]; is64Hex(h) {
			s.current[op] = h
		}
		if h := previous[op]; is64Hex(h) {
			s.previous[op] = h
		}
	}
}

func (s *hashStore) hasAllTargets() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for op := range opHashDefaults {
		if s.current[op] == "" && s.previous[op] == "" {
			return false
		}
	}
	return true
}

// copies current+previous for persistence
func (s *hashStore) snapshot() (current, previous map[string]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	current = make(map[string]string, len(s.current))
	previous = make(map[string]string, len(s.previous))
	for k, v := range s.current {
		current[k] = v
	}
	for k, v := range s.previous {
		previous[k] = v
	}
	return current, previous
}
