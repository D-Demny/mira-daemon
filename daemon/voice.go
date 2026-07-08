package daemon

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// controls the whole on device voice flow. from the wake word ("hey mira") -> ASR -> search -> play
type voiceService struct {
	app *App
	cfg VoiceConfig

	ctx    context.Context
	cancel context.CancelFunc

	mu   sync.Mutex
	gen  uint64
	busy bool

	bannerSeq atomic.Uint64

	// cascade resolver + g2p
	g2p      *g2p
	resolver *cascadeResolver

	// persistent gigaspeech-Zipformer ASR
	sherpa *sherpaSidecar

	// set while the FIRST catalog sync runs
	firstSyncInProgress atomic.Bool

	// wake/mic on-off
	wakeEnabled atomic.Bool
	wakeSignal  chan struct{}
	wakeCancel  atomic.Pointer[context.CancelFunc]

	// playback-aware wake threshold
	playbackActive atomic.Bool
	thrGen         atomic.Uint64
	runningThr     atomic.Uint64
	thrRestart     atomic.Bool
	capturing      atomic.Bool

	// pathfinder hash rotation
	hashRotateGate atomic.Bool
	lastRotate     atomic.Int64
	hashMu         sync.Mutex
	hashBundle     string
	hashFetchedAt  int64
}

// about 10% up or down
const voiceVolumeStep int32 = MaxStateVolume / 10

func newVoiceService(app *App) *voiceService {
	ctx, cancel := context.WithCancel(context.Background())
	v := &voiceService{app: app, cfg: app.cfg.Voice, ctx: ctx, cancel: cancel, wakeSignal: make(chan struct{}, 1)}
	v.wakeEnabled.Store(true)
	return v
}

func (v *voiceService) Start() {
	v.app.log.Infof("voice: service enabled (wake=%v cascade=%v catalog=%v hash_rotate=%v sherpa=%v bin=%s models=%s)",
		v.cfg.Wake, v.cfg.Cascade, v.cfg.CatalogSync, v.cfg.HashRotate, v.cfg.SherpaEnabled, v.cfg.BinDir, v.cfg.ModelDir)
	if v.cfg.Cascade {
		v.startCascade()
	}
	if v.cfg.SherpaEnabled {
		v.sherpa = newSherpaSidecar(v)
		v.sherpa.start() // loads the model in the background
	}
	if v.cfg.HashRotate {
		v.loadHashCache()
		go v.runHashRotate()
	}
	if v.cfg.Wake {
		v.wakeEnabled.Store(voiceMicFromSettings(v.app.GetSettings()))
		go v.runWakeLoop()
	}
}

func (v *voiceService) cacheDir() string {
	if v.cfg.CacheDir != "" {
		return v.cfg.CacheDir
	}
	return filepath.Join(filepath.Dir(strings.TrimRight(v.cfg.ModelDir, "/")), "cache")
}

func (v *voiceService) espeakDataParent() string {
	if v.cfg.EspeakDataDir != "" {
		return v.cfg.EspeakDataDir
	}
	return defaultEspeakDataParent(v.cfg.BinDir)
}

// g2p + resolver
func (v *voiceService) startCascade() {
	espeakBin := v.cfg.EspeakBin
	if espeakBin == "" {
		espeakBin = "espeak-ng"
	}
	v.g2p = newG2P(v.loader(), v.cfg.LibDir, filepath.Join(v.cfg.BinDir, espeakBin), v.espeakDataParent())
	v.resolver = newCascadeResolver(v.g2p, v.cfg.AcceptThreshold)

	syncer := newCatalogSyncer(
		&playerCatalogFetcher{submit: v.app.server.Submit},
		v.g2p, v.cacheDir(), v.app.log,
		func(idx *routedIndex) {
			v.resolver.setIndex(idx)
			v.app.log.Infof("voice: index published (tracks=%d artists=%d playlists=%d albums=%d)",
				len(idx.Tracks), len(idx.Artists), len(idx.Playlists), len(idx.Albums))
		},
	)

	syncer.onPersistedDrift = v.triggerHashRotate

	// use any cached index so voice works without a new sync if needed
	hadIndex := false
	if idx, ok := syncer.loadCachedIndex(); ok {
		hadIndex = true
		v.resolver.setIndex(idx)
		v.app.log.Infof("voice: loaded cached phonetic index (tracks=%d artists=%d playlists=%d albums=%d)",
			len(idx.Tracks), len(idx.Artists), len(idx.Playlists), len(idx.Albums))
	}

	if v.cfg.CatalogSync {
		go v.runCatalogSync(syncer, hadIndex)
	}
}

func (v *voiceService) runCatalogSync(syncer *catalogSyncer, hadIndex bool) {
	if !v.g2p.available(v.ctx) {
		v.app.log.Warn("voice: espeak-ng g2p unavailable, phonetic re-rank disabled, commands fall through to searchDesktop")
		return
	}
	// wait until the player can serve pathfinder requests
	for v.ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(v.ctx, 15*time.Second)
		_, err := v.app.server.Submit(ctx, ApiRequestTypeCatalogPage,
			ApiRequestDataCatalogPage{Kind: catalogKindLiked, Offset: 0, Limit: 1})
		cancel()
		if err == nil {
			break
		}
		if v.ctx.Err() != nil {
			return
		}
		v.app.log.Debugf("voice: catalog sync waiting for session: %v", err)
		select {
		case <-time.After(30 * time.Second):
		case <-v.ctx.Done():
			return
		}
	}
	if hadIndex {
		// voice already works on the cached index, so we run sync in the background now
		if _, _, err := syncer.Refresh(v.ctx); err != nil && v.ctx.Err() == nil {
			v.app.log.Warnf("voice: catalog refresh ended with error: %v", err)
		}
		return
	}
	// first ever sync we show a setting things up dialog with the start splash
	v.firstSyncInProgress.Store(true)
	_, err := syncer.Run(v.ctx)
	v.firstSyncInProgress.Store(false)
	if err != nil && v.ctx.Err() == nil {
		v.app.log.Warnf("voice: catalog sync ended with error: %v", err)
	}
}

func (v *voiceService) Stop() {
	if v.cancel != nil {
		v.cancel()
	}
}

func (v *voiceService) emit(state, text string) {
	v.bannerSeq.Add(1)
	v.app.server.Emit(&ApiEvent{Type: ApiEventTypeVoice, Data: ApiEventDataVoice{State: state, Text: text}})
}

// used to run the voice binaries
func (v *voiceService) loader() string {
	return filepath.Join(v.cfg.LibDir, "ld-linux-aarch64.so.1")
}

// runs the full voice flow
func (v *voiceService) TriggerVoice(ctx context.Context, transcript, clipPath string) (string, error) {
	v.mu.Lock()
	if v.busy {
		v.mu.Unlock()
		return "", fmt.Errorf("voice: busy with another command")
	}
	v.busy = true
	v.gen++
	gen := v.gen
	v.mu.Unlock()
	defer func() { v.mu.Lock(); v.busy = false; v.mu.Unlock() }()

	// "listening" is emitted by the wake loop the instant the wake word wakes
	v.emit("thinking", "")

	// 1. ASR
	if transcript == "" && clipPath != "" {
		if v.sherpa == nil {
			v.flash(gen, "error", "Sorry, I didn't catch that")
			return "", fmt.Errorf("voice: no ASR available (sherpa disabled)")
		}
		t, err := v.sherpa.transcribe(ctx, clipPath)
		if err != nil {
			v.app.log.WithError(err).Warn("voice: ASR failed")
			v.flash(gen, "error", "Sorry, I didn't catch that")
			return "", err
		}
		transcript = t
	}
	if strings.TrimSpace(transcript) == "" {
		v.flash(gen, "error", "Sorry, I didn't catch that")
		return "", fmt.Errorf("voice: empty transcript")
	}

	// 2. resolve
	hyps := []string{transcript}

	var query string
	if v.resolver != nil {
		dec := v.resolver.resolve(ctx, hyps)
		switch dec.Tier {
		case "control":
			return v.handleControl(ctx, gen, dec.Action)
		case "queue":
			// resolved in library queue match
			v.app.log.Infof("voice: transcript=%q -> queue %s (%s, score=%.3f)", transcript, voiceLabel(dec), dec.Uri, dec.Score)
			return v.handleQueue(ctx, gen, dec.Uri, voiceLabel(dec))
		case "abstain":
			// do nothing on purpose
			msg := "Sorry, I didn’t catch that"
			if dec.Kind == kindQueue {
				msg = "Couldn’t find that to queue"
			} else if dec.Kind == kindRandom {
				msg = "No liked songs yet"
			}
			v.app.log.Infof("voice: transcript=%q -> abstain (%s, best=%q score=%.3f)", transcript, dec.Kind, dec.Name, dec.Score)
			v.flash(gen, "error", msg)
			return "", fmt.Errorf("voice: abstained on %q", transcript)
		case "local":
			// in library phonetic match
			if dec.Uri == "" {
				query = searchQueryFromResult(dec)
				v.app.log.Warnf("voice: transcript=%q -> local match %q had no URI, falling back to search", transcript, dec.Name)
			} else {
				label := voiceLabel(dec)
				v.app.log.Infof("voice: transcript=%q -> [%s] %s (%s, score=%.3f)", transcript, dec.Kind, label, dec.Uri, dec.Score)
				return v.playUri(ctx, gen, dec.Uri, label)
			}
		default: // "search"
			v.app.log.Infof("voice: transcript=%q -> searchDesktop re-rank query=%q (best local: %q kind=%s score=%.3f, indexSize=%d)",
				transcript, dec.Query, dec.Name, dec.Kind, dec.Score, dec.IndexSize)
			return v.searchRerankPlay(ctx, gen, transcript, dec)
		}
	} else {
		query = parseVoiceQuery(transcript)
		v.app.log.Infof("voice: transcript=%q -> query=%q", transcript, query)
	}

	if strings.TrimSpace(query) == "" {
		v.flash(gen, "error", "Sorry, I didn't catch that")
		return "", fmt.Errorf("voice: empty query from %q", transcript)
	}
	v.emit("thinking", "") // UI picks a creative word

	// 3. search
	res, err := v.app.server.Submit(ctx, ApiRequestTypeSearch, ApiRequestDataSearch{Query: query})
	if err != nil {
		v.app.log.WithError(err).Warn("voice: search failed")
		v.flash(gen, "error", "Couldn't search right now")
		return "", err
	}
	track, _ := res.(*searchTrackResult)
	if track == nil || track.Uri == "" {
		v.flash(gen, "error", fmt.Sprintf("Couldn't find “%s”", query))
		return "", fmt.Errorf("voice: no track for %q", query)
	}

	label := track.Name
	if track.Artist != "" {
		label = track.Name + " - " + track.Artist
	}

	// 4. play + noti
	return v.playUri(ctx, gen, track.Uri, label)
}

// emits the playing banner
func (v *voiceService) playUri(ctx context.Context, gen uint64, uri, label string) (string, error) {
	v.emit("playing", label)
	shuffle := false
	if _, err := v.app.server.Submit(ctx, ApiRequestTypePlay, ApiRequestDataPlay{Uri: uri, Shuffle: &shuffle}); err != nil {
		v.app.log.WithError(err).Warn("voice: play failed")
		v.flash(gen, "error", "Couldn't start playback")
		return label, err
	}
	v.app.log.Infof("voice: playing %s (%s)", label, uri)
	v.scheduleIdle(gen, 5*time.Second)
	return label, nil
}

// handles the cascade "search" tier
func (v *voiceService) searchRerankPlay(ctx context.Context, gen uint64, transcript string, dec resolveResult) (string, error) {
	query := strings.TrimSpace(dec.Query)
	if query == "" {
		v.flash(gen, "error", "Sorry, I didn't catch that")
		return "", fmt.Errorf("voice: empty search query from %q", transcript)
	}
	v.emit("thinking", "")

	res, err := v.app.server.Submit(ctx, ApiRequestTypeSearch, ApiRequestDataSearch{Query: query, TopN: true})
	if err != nil {
		v.app.log.WithError(err).Warn("voice: search failed")
		v.flash(gen, "error", "Couldn't search right now")
		return "", err
	}
	tracks, _ := res.([]searchTrackResult)
	if len(tracks) == 0 {
		v.flash(gen, "error", fmt.Sprintf("Couldn't find “%s”", query))
		return "", fmt.Errorf("voice: no results for %q", query)
	}

	// rank against what was said
	qTrack := dec.QueryTrack
	if qTrack == "" {
		qTrack = query
	}
	pick, score, ok := v.resolver.rerankSearch(ctx, qTrack, dec.QueryArtist, tracks)
	if !ok {
		v.app.log.Infof("voice: search re-rank ABSTAIN query=%q (closest %q, %q score=%.3f > %.2f, of %d)",
			query, pick.Name, pick.Artist, score, searchRerankFloor, len(tracks))
		v.flash(gen, "error", fmt.Sprintf("Couldn't find “%s”", query))
		return "", fmt.Errorf("voice: search re-rank abstained on %q", transcript)
	}

	label := pick.Name
	if pick.Artist != "" {
		label = pick.Name + " - " + pick.Artist
	}
	v.app.log.Infof("voice: search re-rank -> %s (%s, score=%.3f, of %d results)", label, pick.Uri, score, len(tracks))
	return v.playUri(ctx, gen, pick.Uri, label)
}

// builds the "Song - Artist" banner label
func voiceLabel(d resolveResult) string {
	if d.Artist != "" {
		return d.Name + " - " + d.Artist
	}
	return d.Name
}

// dispatches a recognized control verb
func (v *voiceService) handleControl(ctx context.Context, gen uint64, action string) (string, error) {
	switch action {
	case "resume":
		return v.handleResume(ctx, gen)
	case "volup", "voldown":
		return v.handleVolume(ctx, gen, action)
	}

	var t ApiRequestType
	var banner string
	switch action {
	case "pause":
		t, banner = ApiRequestTypePause, "Paused"
	case "next":
		t, banner = ApiRequestTypeNext, "Skipping"
	case "prev":
		t, banner = ApiRequestTypePrev, "Previous"
	default:
		v.flash(gen, "error", "Sorry, I didn't catch that")
		return "", fmt.Errorf("voice: unknown control action %q", action)
	}
	v.app.log.Infof("voice: control -> %s", action)
	if _, err := v.app.server.Submit(ctx, t, nil); err != nil {
		v.app.log.WithError(err).Warn("voice: control failed")
		v.flash(gen, "error", "Couldn't do that")
		return "", err
	}
	v.flash(gen, "done", banner)
	return action, nil
}

// resumes the active device
func (v *voiceService) handleResume(ctx context.Context, gen uint64) (string, error) {
	v.app.log.Info("voice: control -> resume")
	if _, err := v.app.server.Submit(ctx, ApiRequestTypeResume, nil); err == nil {
		v.flash(gen, "done", "Resumed")
		return "resume", nil
	} else {
		v.app.log.WithError(err).Debug("voice: resume failed; trying resume_last (idle fallback)")
	}
	if _, err := v.app.server.Submit(ctx, ApiRequestTypeResumeLast, nil); err != nil {
		v.app.log.WithError(err).Warn("voice: resume_last failed")
		v.flash(gen, "error", "Nothing to resume")
		return "", err
	}
	v.flash(gen, "done", "Resumed")
	return "resume", nil
}

// handles volume controls
func (v *voiceService) handleVolume(ctx context.Context, gen uint64, action string) (string, error) {
	step := voiceVolumeStep
	banner := "Volume up"
	if action == "voldown" {
		step = -step
		banner = "Volume down"
	}
	v.app.log.Infof("voice: control -> %s (step=%d)", action, step)
	if _, err := v.app.server.Submit(ctx, ApiRequestTypeSetVolume, ApiRequestDataVolume{Volume: step, Relative: true}); err != nil {
		v.app.log.WithError(err).Warn("voice: volume failed")
		v.flash(gen, "error", "Couldn't change volume")
		return "", err
	}
	v.flash(gen, "done", banner)
	return action, nil
}

// adds a resolved in library track to the active devices queue
func (v *voiceService) handleQueue(ctx context.Context, gen uint64, uri, label string) (string, error) {
	v.emit("thinking", "")
	if _, err := v.app.server.Submit(ctx, ApiRequestTypeAddToQueue, uri); err != nil {
		v.app.log.WithError(err).Warn("voice: add_to_queue failed")
		v.flash(gen, "error", "Couldn't queue that")
		return "", err
	}
	v.app.log.Infof("voice: queued %s (%s)", label, uri)
	v.flash(gen, "done", "Queued "+label)
	return label, nil
}

func (v *voiceService) flash(gen uint64, state, text string) {
	v.emit(state, text)
	v.scheduleIdle(gen, 3500*time.Millisecond)
}

func (v *voiceService) scheduleIdle(gen uint64, d time.Duration) {
	seq := v.bannerSeq.Load()
	time.AfterFunc(d, func() {
		if v.bannerSeq.Load() != seq {
			return
		}
		v.mu.Lock()
		cur := v.gen
		v.mu.Unlock()
		if cur == gen {
			v.emit("idle", "")
		}
	})
}

// flips the banner to idle
func (v *voiceService) scheduleClear(d time.Duration) {
	seq := v.bannerSeq.Load()
	time.AfterFunc(d, func() {
		if v.bannerSeq.Load() == seq {
			v.emit("idle", "")
		}
	})
}

// runs the always-on wake-word listener
func (v *voiceService) setWakeEnabled(on bool) {
	if v.wakeEnabled.Swap(on) == on {
		return // no change
	}
	if on {
		v.app.log.Info("voice: mic enabled")
		select {
		case v.wakeSignal <- struct{}{}:
		default:
		}
	} else {
		v.app.log.Info("voice: mic disabled")
		if c := v.wakeCancel.Load(); c != nil {
			(*c)() // kill the running oww_wake
		}
	}
}

const wakeThresholdDebounce = 10 * time.Second

// the wake threshold for the current playback state
func (v *voiceService) wakeThreshold() float64 {
	if v.playbackActive.Load() && v.cfg.WakeThresholdPlaying > 0 {
		return v.cfg.WakeThresholdPlaying
	}
	return v.cfg.WakeThreshold
}

func (v *voiceService) notifyPlayback(playing bool) {
	if !v.cfg.Wake {
		return // no listener to retune
	}
	if v.playbackActive.Swap(playing) == playing {
		return // no change
	}
	gen := v.thrGen.Add(1)
	time.AfterFunc(wakeThresholdDebounce, func() { v.applyWakeThreshold(gen) })
}

// restarts the wake listener
func (v *voiceService) applyWakeThreshold(gen uint64) {
	if v.thrGen.Load() != gen {
		return // a newer flip superseded this one
	}
	if v.ctx.Err() != nil || !v.wakeEnabled.Load() {
		return
	}
	want := v.wakeThreshold()
	if math.Float64frombits(v.runningThr.Load()) == want {
		return
	}
	// a command is mid flight
	v.mu.Lock()
	busy := v.busy
	v.mu.Unlock()
	if busy || v.capturing.Load() {
		time.AfterFunc(2*time.Second, func() { v.applyWakeThreshold(gen) })
		return
	}
	v.app.log.Infof("voice: wake threshold -> %.2f (playing=%v)", want, v.playbackActive.Load())
	v.thrRestart.Store(true)
	if c := v.wakeCancel.Load(); c != nil {
		(*c)()
	}
}

func (v *voiceService) runWakeLoop() {
	mel := filepath.Join(v.cfg.ModelDir, "melspectrogram.tflite")
	emb := filepath.Join(v.cfg.ModelDir, "embedding_model.tflite")
	wake := filepath.Join(v.cfg.ModelDir, "hey_mira.tflite")

	for v.ctx.Err() == nil {
		// mic disabled
		if !v.wakeEnabled.Load() {
			select {
			case <-v.wakeSignal:
			case <-v.ctx.Done():
				return
			}
			continue
		}

		thr := v.wakeThreshold()
		v.runningThr.Store(math.Float64bits(thr))

		runCtx, cancel := context.WithCancel(v.ctx)
		v.wakeCancel.Store(&cancel)
		cmd := exec.CommandContext(runCtx, v.loader(), "--library-path", v.cfg.LibDir,
			filepath.Join(v.cfg.BinDir, "oww_wake"), mel, emb, wake,
			"--mic", v.cfg.MicDevice, "--threshold", fmt.Sprintf("%.2f", thr))

		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			v.app.log.WithError(err).Error("voice: wake stdout pipe")
			cancel()
			v.wakeCancel.Store(nil)
			time.Sleep(time.Second)
			continue
		}
		if stderr, err := cmd.StderrPipe(); err == nil {
			go drainLog(v, stderr)
		}
		if err := cmd.Start(); err != nil {
			v.app.log.WithError(err).Error("voice: wake listener start failed")
			cancel()
			v.wakeCancel.Store(nil)
			time.Sleep(2 * time.Second)
			continue
		}
		v.app.log.Info("voice: wake listener started")
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			// wake fires the instant "hey mira" is detected
			if strings.HasPrefix(line, "WAKE") {
				v.app.log.Info("voice: wake word detected")
				v.capturing.Store(true)
				v.emit("listening", "")
				// if no CLIP ever follows (silence timeout, capture death)
				// nothing else would dismiss the banner
				v.scheduleClear(15 * time.Second)
				continue
			}
			if clip, ok := strings.CutPrefix(line, "CLIP "); ok {
				v.capturing.Store(false)
				clip = strings.TrimSpace(clip)
				go func() {
					if _, err := v.TriggerVoice(v.ctx, "", clip); err != nil {
						v.app.log.WithError(err).Debug("voice: wake-triggered flow failed")
					}
				}()
			}
		}
		v.capturing.Store(false)
		_ = cmd.Wait()
		cancel()
		v.wakeCancel.Store(nil)
		if v.ctx.Err() != nil {
			return
		}
		if !v.wakeEnabled.Load() {
			v.app.log.Info("voice: wake listener stopped (mic off)")
			continue
		}
		if v.thrRestart.Swap(false) {
			continue
		}
		v.app.log.Warn("voice: wake listener exited; restarting in 1s")
		time.Sleep(time.Second)
	}
}

var voiceNonWord = regexp.MustCompile(`[^a-z0-9 ]+`)

func parseVoiceQuery(t string) string {
	words := strings.Fields(voiceNonWord.ReplaceAllString(strings.ToLower(t), " "))
	for i, w := range words {
		if w == "play" {
			words = words[i+1:]
			for j, w2 := range words {
				if w2 == "play" {
					words = words[:j]
					break
				}
			}
			break
		}
	}
	for len(words) > 0 && (words[0] == "the" || words[0] == "some" || words[0] == "a") {
		words = words[1:]
	}
	return strings.Join(words, " ")
}

// trims an ASR transcript
func cleanTranscript(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.TrimSpace(s), "\n", " "))
}
