package daemon

import (
	"context"
	"math/rand"
	"regexp"
	"strings"
	"sync"
)

// fall back to searching if score is above this (assume its out of library)
const cascadeAcceptDefault = 0.42

// accept limit higher when we have the right artists
const cascadeTrackAnchoredFloor = 0.60

// acceptance threshold when no "by" keyword
const bareFloor = 0.30

// floor for search fallback, dont play if above this
const searchRerankFloor = 0.40

// threshold for verb mishears
const verbMatchThreshold = 0.34

const (
	weightTrack  = 1.0
	weightArtist = 0.5
)

const bareMaxPrefixWords = 6

const barePrefixPenalty = 0.01

const (
	kindTrack    = "track"
	kindArtist   = "artist"
	kindPlaylist = "playlist"
	kindAlbum    = "album"
)

const (
	kindControl    = "control"
	kindBare       = "bare"
	kindQueue      = "queue"
	kindRandom     = "random"
	kindPlayLiked  = "playliked"
	kindCollection = "collection"
)

// liked songs as a playable context
const likedCollectionUri = "spotify:collection:tracks"

type indexEntry struct {
	Name      string `json:"name"`
	Artist    string `json:"artist,omitempty"`
	Uri       string `json:"uri"`
	Ipa       string `json:"ipa"`
	ArtistIpa string `json:"artist_ipa,omitempty"`
}

// holds the four catalog indexes
type routedIndex struct {
	Tracks    []indexEntry `json:"tracks"`
	Artists   []indexEntry `json:"artists"`
	Playlists []indexEntry `json:"playlists"`
	Albums    []indexEntry `json:"albums"`
	Liked     []indexEntry `json:"liked,omitempty"`
}

type controlPhrase struct {
	phrase string
	action string // pause/resume/next/prev/volup/voldown
}

var controlPhrases = []controlPhrase{
	{"turn it up", "volup"},
	{"turn it down", "voldown"},
	{"volume up", "volup"},
	{"volume down", "voldown"},
	{"turn up", "volup"},
	{"turn down", "voldown"},
	{"go back", "prev"},
	{"pause", "pause"},
	{"stop", "pause"},
	{"resume", "resume"},
	{"skip", "next"},
	{"next", "next"},
	{"previous", "prev"},
	{"back", "prev"},
}

// only words allowed to follow a control phrase
var controlParticles = map[string]bool{
	"this": true, "that": true, "it": true, "song": true, "track": true,
	"please": true, "now": true, "one": true, "the": true, "ahead": true,
	"again": true,
}

// common mishears for the play verb
var leadPlayVariants = map[string]bool{
	"clay": true, "pray": true, "slay": true, "played": true, "plays": true,
	"prey": true, "flay": true, "blay": true, "plei": true, "pleh": true,
}

// filler words stripped from a name slot
var fillerWords = map[string]bool{
	"play": true, "please": true, "the": true, "a": true, "an": true, "of": true,
	"for": true, "his": true, "her": true, "to": true, "put": true, "on": true,
	"some": true, "my": true, "music": true, "song": true, "track": true, "by": true,
}

var resolverNonWord = regexp.MustCompile(`[^\w\s]+`)
var resolverWhitespace = regexp.MustCompile(`\s+`)

func resolverNormalize(h string) string {
	t := resolverNonWord.ReplaceAllString(strings.ToLower(h), " ")
	return strings.TrimSpace(resolverWhitespace.ReplaceAllString(t, " "))
}

func stripFiller(s string) string {
	if s == "" {
		return ""
	}
	var kept []string
	for _, w := range strings.Fields(s) {
		if !fillerWords[w] {
			kept = append(kept, w)
		}
	}
	return strings.TrimSpace(strings.Join(kept, " "))
}

type voiceIntent struct {
	kind   string
	action string
	slots  map[string]string
}

// maps a transcript to an intent + slots
func (c *cascadeResolver) classify(ctx context.Context, h string) voiceIntent {
	t := resolverNormalize(h)
	if t == "" {
		return voiceIntent{kind: kindBare, slots: map[string]string{}}
	}
	words := strings.Fields(t)

	if action, ok := c.matchControl(ctx, words); ok {
		return voiceIntent{kind: kindControl, action: action}
	}

	if len(words) > 1 && leadPlayVariants[words[0]] {
		words[0] = "play"
		t = strings.Join(words, " ")
	}

	if c.matchesLiked(t, words) {
		return voiceIntent{kind: kindPlayLiked, slots: map[string]string{}}
	}
	if matchesRandom(t) {
		return voiceIntent{kind: kindRandom, slots: map[string]string{}}
	}
	if name, ok := c.queueName(ctx, t, words); ok {
		in := classifyGrammar(name)
		qslots := map[string]string{}
		if in.kind == kindTrack {
			qslots["track"], qslots["artist"] = in.slots["track"], in.slots["artist"]
		} else {
			qslots["name"] = in.slots["name"]
		}
		return voiceIntent{kind: kindQueue, slots: qslots}
	}

	return classifyGrammar(t)
}

func classifyGrammar(t string) voiceIntent {
	t = strings.TrimSpace(t)
	if t == "" {
		return voiceIntent{kind: kindBare, slots: map[string]string{}}
	}

	// track: "X by Y"
	if idx := strings.Index(t, " by "); idx >= 0 {
		left := t[:idx]
		right := t[idx+len(" by "):]
		return voiceIntent{kind: kindTrack, slots: map[string]string{
			"track": stripFiller(left), "artist": stripFiller(right),
		}}
	}

	// artist: "X radio"/"X discography"/"everything by X"/"X songs"
	if strings.HasSuffix(t, " radio") || strings.HasSuffix(t, " discography") ||
		strings.Contains(t, " everything by ") || strings.HasSuffix(t, " songs") {
		name := t
		for _, suf := range []string{" radio", " discography", " songs"} {
			name = strings.TrimSuffix(name, suf)
		}
		name = strings.ReplaceAll(name, "everything by", "")
		return voiceIntent{kind: kindArtist, slots: map[string]string{"name": stripFiller(name)}}
	}

	// playlist
	if strings.Contains(t, "playlist") || strings.HasPrefix(t, "play my ") {
		name := strings.ReplaceAll(t, "playlist", "")
		return voiceIntent{kind: kindPlaylist, slots: map[string]string{"name": stripFiller(name)}}
	}

	// album
	if strings.Contains(t, "album") {
		name := strings.ReplaceAll(t, "album", "")
		return voiceIntent{kind: kindAlbum, slots: map[string]string{"name": stripFiller(name)}}
	}

	return voiceIntent{kind: kindBare, slots: map[string]string{"name": stripFiller(t)}}
}

// detects a leading control phrase
func (c *cascadeResolver) matchControl(ctx context.Context, words []string) (string, bool) {
	if len(words) == 0 {
		return "", false
	}
	for _, e := range controlPhrases {
		pw := strings.Fields(e.phrase)
		n := len(pw)
		if len(words) < n {
			continue
		}
		if c.wordsMatch(ctx, words[:n], pw) && restIsFiller(words[n:]) {
			return e.action, true
		}
	}
	return "", false
}

func (c *cascadeResolver) wordsMatch(ctx context.Context, lead, pw []string) bool {
	for i := range pw {
		if !c.wordMatch(ctx, lead[i], pw[i]) {
			return false
		}
	}
	return true
}

func (c *cascadeResolver) wordMatch(ctx context.Context, a, b string) bool {
	if a == b {
		return true
	}
	if negPrefix(a) != negPrefix(b) {
		return false
	}
	ia := []rune(c.g2p.ipa(ctx, a))
	ib := []rune(c.g2p.ipa(ctx, b))
	return len(ia) > 0 && len(ib) > 0 && normDist(ia, ib) <= verbMatchThreshold
}

func negPrefix(w string) bool {
	return strings.HasPrefix(w, "un") || strings.HasPrefix(w, "dis")
}

func restIsFiller(rest []string) bool {
	for _, w := range rest {
		if !controlParticles[w] {
			return false
		}
	}
	return true
}

// matchesLiked detects "play (my) liked songs"/"(my) liked songs"
func (c *cascadeResolver) matchesLiked(t string, words []string) bool {
	hasLiked := false
	for _, w := range words {
		if w == "liked" || w == "licked" {
			hasLiked = true
			break
		}
	}
	if !hasLiked {
		return false
	}
	for _, w := range words {
		if !likedFiller[w] {
			return false
		}
	}
	return true
}

var likedFiller = map[string]bool{
	"play": true, "my": true, "the": true, "a": true, "some": true, "of": true,
	"to": true, "songs": true, "song": true, "tracks": true, "track": true,
	"music": true, "liked": true, "licked": true, "like": true, "list": true,
	"all": true, "collection": true, "saved": true, "favourites": true, "favorites": true,
}

// matchesRandom detects "play a random song"/"surprise me"/bare "random"
func matchesRandom(t string) bool {
	if t == "surprise me" || t == "surprise" {
		return true
	}
	if !strings.Contains(t, "random") {
		return false
	}
	for _, w := range strings.Fields(t) {
		if !randomFiller[w] {
			return false
		}
	}
	return true
}

var randomFiller = map[string]bool{
	"play": true, "a": true, "an": true, "some": true, "the": true, "my": true,
	"song": true, "songs": true, "track": true, "tracks": true, "music": true,
	"something": true, "any": true, "random": true, "anything": true, "shuffle": true,
}

// extracts the name slot for a queue command
func (c *cascadeResolver) queueName(ctx context.Context, t string, words []string) (string, bool) {
	// "add X to (the/my) queue"
	if words[0] == "add" && strings.HasSuffix(t, " queue") {
		inner := strings.TrimSuffix(t, " queue")
		inner = strings.TrimPrefix(inner, "add ")
		for _, suf := range []string{" to the", " to my", " to"} {
			inner = strings.TrimSuffix(inner, suf)
		}
		if inner = strings.TrimSpace(inner); inner != "" {
			return inner, true
		}
	}
	if len(words) > 1 && c.isQueueVerb(ctx, words[0]) {
		return strings.Join(words[1:], " "), true
	}
	return "", false
}

func (c *cascadeResolver) isQueueVerb(ctx context.Context, w string) bool {
	if w == "queue" || w == "cue" {
		return true
	}
	a := []rune(c.g2p.ipa(ctx, w))
	b := []rune(c.g2p.ipa(ctx, "queue"))
	return len(a) > 0 && len(b) > 0 && normDist(a, b) <= verbMatchThreshold
}

func levRunes(a, b []rune) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	cur := make([]int, lb+1)
	for i := 1; i <= la; i++ {
		cur[0] = i
		ca := a[i-1]
		for j := 1; j <= lb; j++ {
			cost := 1
			if ca == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[lb]
}

func normDist(a, b []rune) float64 {
	d := levRunes(a, b)
	m := max(len(a), len(b), 1)
	return float64(d) / float64(m)
}

// resolver

type resolveResult struct {
	Tier        string // control/local/queue/abstain/search
	Kind        string // track/artist/playlist/album/collection
	Action      string // control action (pause/resume/next/prev/shuffle/volup/voldown)
	Name        string
	Artist      string
	Uri         string
	Score       float64
	Query       string
	QueryTrack  string
	QueryArtist string
	Via         string
	IndexSize   int
}

type cascadeResolver struct {
	g2p    *g2p
	accept float64

	mu  sync.RWMutex
	idx *routedIndex
}

func newCascadeResolver(g *g2p, accept float64) *cascadeResolver {
	if accept <= 0 {
		accept = cascadeAcceptDefault
	}
	return &cascadeResolver{g2p: g, accept: accept, idx: &routedIndex{}}
}

func (c *cascadeResolver) setIndex(idx *routedIndex) {
	c.mu.Lock()
	c.idx = idx
	c.mu.Unlock()
}

func (c *cascadeResolver) index() *routedIndex {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.idx
}

func (c *cascadeResolver) floorFor(m *localCandidate) float64 {
	switch {
	case m.kind == kindTrack && m.anchored && cascadeTrackAnchoredFloor > c.accept:
		return cascadeTrackAnchoredFloor
	case m.bare && bareFloor < c.accept:
		return bareFloor
	default:
		return c.accept
	}
}

// intermediate scored match before the threshold gate.
type localCandidate struct {
	score    float64
	kind     string
	name     string
	artist   string
	uri      string
	anchored bool
	bare     bool
}

// resolve runs the cascade over the transcript hypotheses
func (c *cascadeResolver) resolve(ctx context.Context, hyps []string) resolveResult {
	idx := c.index()

	controlVotes := map[string]int{}
	var firstControlHyp string
	var specialHyp string
	playLiked, random := false, false

	var queueSeen bool
	var bestQueueAccepted, bestQueueAny *localCandidate
	var queueHyp string

	var bestAccepted, bestAny *localCandidate
	var bestHyp, fallbackQuery, fallbackHyp string
	var fallbackTrack, fallbackArtist string
	consider := func(m *localCandidate, h string, acc, any **localCandidate, accHyp *string) {
		if m == nil {
			return
		}
		if *any == nil || m.score < (*any).score {
			*any = m
		}
		if m.score <= c.floorFor(m) && (*acc == nil || m.score < (*acc).score) {
			*acc = m
			*accHyp = h
		}
	}

	for _, h := range hyps {
		if strings.TrimSpace(h) == "" {
			continue
		}
		in := c.classify(ctx, h)
		switch in.kind {
		case kindControl:
			controlVotes[in.action]++
			if firstControlHyp == "" {
				firstControlHyp = h
			}
			continue
		case kindPlayLiked:
			playLiked = true
			if specialHyp == "" {
				specialHyp = h
			}
			continue
		case kindRandom:
			random = true
			if specialHyp == "" {
				specialHyp = h
			}
			continue
		case kindQueue:
			queueSeen = true
			if queueHyp == "" {
				queueHyp = h
			}
			consider(c.matchForIntent(ctx, idx, in), h, &bestQueueAccepted, &bestQueueAny, &queueHyp)
			continue
		}

		if fallbackQuery == "" {
			// small clean for the search query
			if lq, lt, la := searchTermsLight(h); lq != "" {
				fallbackQuery, fallbackHyp = lq, h
				fallbackTrack, fallbackArtist = lt, la
			}
		}
		consider(c.matchForIntent(ctx, idx, in), h, &bestAccepted, &bestAny, &bestHyp)
	}

	// Priority 1: queue
	if queueSeen {
		if bestQueueAccepted != nil {
			return resolveResult{Tier: "queue", Kind: bestQueueAccepted.kind, Name: bestQueueAccepted.name,
				Artist: bestQueueAccepted.artist, Uri: bestQueueAccepted.uri, Score: bestQueueAccepted.score, Via: queueHyp}
		}
		r := resolveResult{Tier: "abstain", Kind: kindQueue, Via: queueHyp}
		if bestQueueAny != nil {
			r.Score, r.Name = bestQueueAny.score, bestQueueAny.name
		}
		return r
	}

	// Priority 2: "play my liked songs".
	if playLiked {
		return resolveResult{Tier: "local", Kind: kindCollection, Uri: likedCollectionUri, Name: "Liked Songs", Via: specialHyp}
	}

	// Priority 3: random from liked songs
	if random {
		if e := c.randomLiked(); e != nil {
			return resolveResult{Tier: "local", Kind: kindTrack, Uri: e.Uri, Name: e.Name, Artist: e.Artist, Via: specialHyp}
		}
		return resolveResult{Tier: "abstain", Kind: kindRandom, Via: specialHyp}
	}

	// Priority 4: content local match.
	if bestAccepted != nil {
		return resolveResult{Tier: "local", Kind: bestAccepted.kind, Name: bestAccepted.name,
			Artist: bestAccepted.artist, Uri: bestAccepted.uri, Score: bestAccepted.score, Via: bestHyp}
	}

	// Priority 5: control
	if len(controlVotes) > 0 {
		return resolveResult{Tier: "control", Action: topVote(controlVotes), Via: firstControlHyp}
	}

	// Priority 6: search
	q := fallbackQuery
	if q == "" && len(hyps) > 0 {
		q = parseVoiceQuery(hyps[0])
		fallbackHyp = hyps[0]
	}
	r := resolveResult{Tier: "search", Query: q, QueryTrack: fallbackTrack, QueryArtist: fallbackArtist, Via: fallbackHyp,
		IndexSize: len(idx.Tracks) + len(idx.Artists) + len(idx.Playlists) + len(idx.Albums)}
	if bestAny != nil {
		r.Score, r.Name, r.Kind = bestAny.score, bestAny.name, bestAny.kind
	}
	return r
}

// matchForIntent runs the right matcher for a content/queue intent
func (c *cascadeResolver) matchForIntent(ctx context.Context, idx *routedIndex, in voiceIntent) *localCandidate {
	switch in.kind {
	case kindTrack:
		return c.matchTrack(ctx, idx, in.slots["track"], in.slots["artist"])
	case kindQueue:
		if t := in.slots["track"]; t != "" {
			return c.matchTrack(ctx, idx, t, in.slots["artist"])
		}
		return c.matchBare(ctx, idx, in.slots["name"])
	case kindArtist:
		return lowerOf(c.matchSimple(ctx, idx.Artists, in.slots["name"], kindArtist), c.matchBare(ctx, idx, in.slots["name"]))
	case kindPlaylist:
		return lowerOf(c.matchSimple(ctx, idx.Playlists, in.slots["name"], kindPlaylist), c.matchBare(ctx, idx, in.slots["name"]))
	case kindAlbum:
		return lowerOf(c.matchSimple(ctx, idx.Albums, in.slots["name"], kindAlbum), c.matchBare(ctx, idx, in.slots["name"]))
	default:
		return c.matchBare(ctx, idx, in.slots["name"])
	}
}

// lowerOf returns the lower-scoring of two candidates
func lowerOf(a, b *localCandidate) *localCandidate {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case b.score < a.score:
		return b
	default:
		return a
	}
}

// topVote returns the most-voted control action
func topVote(votes map[string]int) string {
	best, n := "", -1
	for _, e := range controlPhrases {
		if v := votes[e.action]; v > n {
			best, n = e.action, v
		}
	}
	return best
}

// returns a random liked song
func (c *cascadeResolver) randomLiked() *indexEntry {
	idx := c.index()
	if len(idx.Liked) == 0 {
		return nil
	}
	return &idx.Liked[rand.Intn(len(idx.Liked))]
}

func (c *cascadeResolver) matchTrack(ctx context.Context, idx *routedIndex, track, artist string) *localCandidate {
	if track == "" || len(idx.Tracks) == 0 {
		return nil
	}
	qt := []rune(c.g2p.ipa(ctx, track))
	if len(qt) == 0 {
		return nil
	}
	var qa []rune
	if artist != "" {
		qa = []rune(c.g2p.ipa(ctx, artist))
	}
	var best *localCandidate
	for i := range idx.Tracks {
		e := &idx.Tracks[i]
		td := normDist(qt, []rune(e.Ipa))
		ad := 0.0
		if len(qa) > 0 && e.ArtistIpa != "" {
			ad = normDist(qa, []rune(e.ArtistIpa))
		}
		sc := weightTrack*td + weightArtist*ad
		if best == nil || sc < best.score {
			best = &localCandidate{score: sc, kind: kindTrack, name: e.Name, artist: e.Artist, uri: e.Uri, anchored: len(qa) > 0}
		}
	}
	return best
}

// nearest entry by name phonemes
func (c *cascadeResolver) matchSimple(ctx context.Context, entries []indexEntry, name, kind string) *localCandidate {
	if name == "" || len(entries) == 0 {
		return nil
	}
	qi := []rune(c.g2p.ipa(ctx, name))
	if len(qi) == 0 {
		return nil
	}
	var best *localCandidate
	for i := range entries {
		e := &entries[i]
		sc := normDist(qi, []rune(e.Ipa))
		if best == nil || sc < best.score {
			best = &localCandidate{score: sc, kind: kind, name: e.Name, artist: e.Artist, uri: e.Uri}
		}
	}
	return best
}

// "play X" (no artist append), try every index and take the best
func (c *cascadeResolver) matchBare(ctx context.Context, idx *routedIndex, name string) *localCandidate {
	if name == "" {
		return nil
	}
	var best *localCandidate
	consider := func(m *localCandidate) {
		if m != nil && (best == nil || m.score < best.score) {
			best = m
		}
	}
	words := strings.Fields(name)
	if len(words) > bareMaxPrefixWords {
		words = words[:bareMaxPrefixWords]
	}
	for n := len(words); n >= 1; n-- {
		qi := []rune(c.g2p.ipa(ctx, strings.Join(words[:n], " ")))
		if len(qi) == 0 {
			continue
		}
		penalty := float64(len(words)-n) * barePrefixPenalty
		for i := range idx.Tracks {
			e := &idx.Tracks[i]
			sc := normDist(qi, []rune(e.Ipa)) + penalty
			consider(&localCandidate{score: sc, kind: kindTrack, name: e.Name, artist: e.Artist, uri: e.Uri})
		}
	}

	// artists/playlists/albums match the full query
	consider(c.matchSimple(ctx, idx.Artists, name, kindArtist))
	consider(c.matchSimple(ctx, idx.Playlists, name, kindPlaylist))
	consider(c.matchSimple(ctx, idx.Albums, name, kindAlbum))
	if best != nil {
		best.bare = true
	}
	return best
}

// build a search query from a local match that lacked a URI
func searchQueryFromResult(d resolveResult) string {
	if d.Artist != "" {
		return strings.TrimSpace(d.Name + " " + d.Artist)
	}
	return d.Name
}

// builds the searchDesktop query + rerank based on the transcript
func searchTermsLight(h string) (query, track, artist string) {
	words := strings.Fields(resolverNormalize(h))
	if len(words) > 0 && (words[0] == "play" || leadPlayVariants[words[0]]) {
		words = words[1:]
	}
	bi := -1
	for i, w := range words {
		if w == "by" {
			bi = i
			break
		}
	}
	if bi >= 0 {
		track = strings.TrimSpace(strings.Join(words[:bi], " "))
		artist = strings.TrimSpace(strings.Join(words[bi+1:], " "))
		query = strings.TrimSpace(track + " " + artist)
	} else {
		track = strings.TrimSpace(strings.Join(words, " "))
		query = track
	}
	return query, track, artist
}

// basically cascade for search
func (c *cascadeResolver) rerankSearch(ctx context.Context, qTrack, qArtist string, results []searchTrackResult) (best searchTrackResult, score float64, ok bool) {
	if qTrack == "" || len(results) == 0 {
		return searchTrackResult{}, 0, false
	}
	qt := []rune(c.g2p.ipa(ctx, qTrack))
	if len(qt) == 0 {
		return searchTrackResult{}, 0, false
	}
	var qa []rune
	if qArtist != "" {
		qa = []rune(c.g2p.ipa(ctx, qArtist))
	}
	bestIdx := -1
	var bestScore float64
	for i := range results {
		td := normDist(qt, []rune(c.g2p.ipa(ctx, results[i].Name)))
		sc := weightTrack * td
		w := weightTrack
		if len(qa) > 0 && results[i].Artist != "" {
			sc += weightArtist * normDist(qa, []rune(c.g2p.ipa(ctx, results[i].Artist)))
			w += weightArtist
		}
		sc /= w
		if bestIdx == -1 || sc < bestScore {
			bestIdx, bestScore = i, sc
		}
	}
	if bestIdx == -1 {
		return searchTrackResult{}, 0, false
	}
	return results[bestIdx], bestScore, bestScore <= searchRerankFloor
}
