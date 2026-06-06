package daemon

import (
	"encoding/json"
	"testing"

	librespot "github.com/devgianlu/go-librespot"
)

func newTestSecondaryProvider() *secondaryLyricProvider {
	return &secondaryLyricProvider{log: &librespot.NullLogger{}}
}

// parseLRC, synced-lyric file parser

func TestParseLRC_StandardMillisecondsPrecision(t *testing.T) {
	t.Parallel()

	// LRC with 3-digit fractional seconds should treated as milliseconds
	result, err := parseLRC(`[01:23.456]Hello`)
	if err != nil {
		t.Fatalf("parseLRC: %v", err)
	}
	if got, want := result.SyncType, "LINE_SYNCED"; got != want {
		t.Errorf("SyncType: got %q want %q", got, want)
	}
	if len(result.Lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(result.Lines))
	}
	if got, want := result.Lines[0].StartTimeMs, "83456"; got != want {
		t.Errorf("StartTimeMs: got %q want %q", got, want)
	}
	if got, want := result.Lines[0].Words, "Hello"; got != want {
		t.Errorf("Words: got %q want %q", got, want)
	}
}

func TestParseLRC_TwoDigitCentisecondsAreScaledTimes10(t *testing.T) {
	t.Parallel()

	// 2-digit fractional *10 to get ms
	result, err := parseLRC(`[01:23.45]Hello`)
	if err != nil {
		t.Fatalf("parseLRC: %v", err)
	}
	if got, want := result.Lines[0].StartTimeMs, "83450"; got != want {
		t.Errorf("StartTimeMs: got %q want %q (should be centiseconds × 10)", got, want)
	}
}

func TestParseLRC_SkipsMetadataBlankAndMalformedLines(t *testing.T) {
	t.Parallel()

	// metadata lines that dont mathch the timestamp regex get skipped
	input := `[ti:Song Title]
[ar:Artist Name]
[al:Album]

[00:01.000]First line
some garbage
[00:05.500]Second line

[00:10.250]Third line
`
	result, err := parseLRC(input)
	if err != nil {
		t.Fatalf("parseLRC: %v", err)
	}
	if len(result.Lines) != 3 {
		t.Fatalf("expected 3 lines, got %d (metadata/blanks should be filtered): %+v",
			len(result.Lines), result.Lines)
	}
	if got, want := result.Lines[0].StartTimeMs, "1000"; got != want {
		t.Errorf("line[0] StartTimeMs: got %q want %q", got, want)
	}
	if got, want := result.Lines[1].Words, "Second line"; got != want {
		t.Errorf("line[1] Words: got %q want %q", got, want)
	}
	if got, want := result.Lines[2].StartTimeMs, "10250"; got != want {
		t.Errorf("line[2] StartTimeMs: got %q want %q", got, want)
	}
}

func TestParseLRC_EmptyInputReturnsError(t *testing.T) {
	t.Parallel()

	if _, err := parseLRC(""); err == nil {
		t.Error("parseLRC(\"\") should return error, got nil")
	}
}

func TestParseLRC_AllMalformedReturnsError(t *testing.T) {
	t.Parallel()

	// all lines fail the regex so no valid lyric lines collected leads to error
	if _, err := parseLRC("not\nLRC\nformat"); err == nil {
		t.Error("parseLRC of all-malformed input should return error, got nil")
	}
}

func TestParseLRC_PreservesUnicodeAndSpecialChars(t *testing.T) {
	t.Parallel()

	// lyrics span every language
	input := `[00:01.000]こんにちは ♪ café - naïve résumé`
	result, err := parseLRC(input)
	if err != nil {
		t.Fatalf("parseLRC: %v", err)
	}
	if got, want := result.Lines[0].Words, "こんにちは ♪ café - naïve résumé"; got != want {
		t.Errorf("Words: got %q want %q", got, want)
	}
}

// plainTextToResult - unsynced lyrics helper

func TestPlainTextToResult_MultilineYieldsUnsyncedWithAllNonEmptyLines(t *testing.T) {
	t.Parallel()

	// Every output line carries StartTimeMs="0" 
	input := "First line\nSecond line\nThird line"
	result := plainTextToResult(input)

	if result == nil {
		t.Fatal("plainTextToResult returned nil for valid input")
	}
	if got, want := result.SyncType, "UNSYNCED"; got != want {
		t.Errorf("SyncType: got %q want %q", got, want)
	}
	if len(result.Lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(result.Lines))
	}
	for i, line := range result.Lines {
		if line.StartTimeMs != "0" {
			t.Errorf("line[%d] StartTimeMs: got %q want %q (unsynced must use 0)",
				i, line.StartTimeMs, "0")
		}
	}
	if got, want := result.Lines[1].Words, "Second line"; got != want {
		t.Errorf("line[1] Words: got %q want %q", got, want)
	}
}

func TestPlainTextToResult_EmptyInputReturnsNil(t *testing.T) {
	t.Parallel()

	if result := plainTextToResult(""); result != nil {
		t.Errorf("plainTextToResult(\"\") should return nil, got %+v", result)
	}
}

func TestPlainTextToResult_AllWhitespaceReturnsNil(t *testing.T) {
	t.Parallel()

	// whitespace-only input returns nil
	if result := plainTextToResult("   \n\t\n  \n"); result != nil {
		t.Errorf("plainTextToResult of whitespace-only should return nil, got %+v", result)
	}
}

// builds the deeply-nested wrapper the secondary source returns
func secondaryEnvelope(statusCode int, macroCalls map[string]any) []byte {
	body, _ := json.Marshal(map[string]any{
		"message": map[string]any{
			"header": map[string]any{"status_code": statusCode},
			"body": map[string]any{
				"macro_calls": macroCalls,
			},
		},
	})
	return body
}

// secondarySubtitleCall builds a successful track.subtitles.get 
func secondarySubtitleCall(subtitleBody string) map[string]any {
	return map[string]any{
		"message": map[string]any{
			"header": map[string]any{"status_code": 200},
			"body": map[string]any{
				"subtitle_list": []any{
					map[string]any{
						"subtitle": map[string]any{
							"subtitle_body": subtitleBody,
						},
					},
				},
			},
		},
	}
}

func TestParseSecondaryResponse_HappyPathSyncedLyrics(t *testing.T) {
	t.Parallel()

	body := secondaryEnvelope(200, map[string]any{
		"track.subtitles.get": secondarySubtitleCall(
			`[{"text":"L0","time":{"total":0}},{"text":"L1","time":{"total":1.5}}]`,
		),
	})

	result, err := newTestSecondaryProvider().parseResponse(body)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if got, want := result.SyncType, "LINE_SYNCED"; got != want {
		t.Errorf("SyncType: got %q want %q", got, want)
	}
	if len(result.Lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(result.Lines))
	}
	if got, want := result.Lines[1].StartTimeMs, "1500"; got != want {
		t.Errorf("Lines[1].StartTimeMs: got %q want %q", got, want)
	}
}

func TestParseSecondaryResponse_FallsBackToPlainWhenSubtitlesFail(t *testing.T) {
	t.Parallel()

	// subtitle macro has the "no synced", should fall through to plain
	subtitleCallWithArrayBody := map[string]any{
		"message": map[string]any{
			"header": map[string]any{"status_code": 200},
			"body":   []any{},
		},
	}
	plainCall := map[string]any{
		"message": map[string]any{
			"header": map[string]any{"status_code": 200},
			"body": map[string]any{
				"lyrics": map[string]any{
					"lyrics_body": "Verse one\nVerse two",
				},
			},
		},
	}

	body := secondaryEnvelope(200, map[string]any{
		"track.subtitles.get": subtitleCallWithArrayBody,
		"track.lyrics.get":    plainCall,
	})

	result, err := newTestSecondaryProvider().parseResponse(body)
	if err != nil {
		t.Fatalf("parseResponse: %v", err)
	}
	if got, want := result.SyncType, "UNSYNCED"; got != want {
		t.Errorf("SyncType: got %q want %q (subtitles failed, should fall back to UNSYNCED plain)", got, want)
	}
	if len(result.Lines) != 2 {
		t.Fatalf("expected 2 unsynced lines, got %d", len(result.Lines))
	}
}

func TestParseSecondaryResponse_TopLevelNon200StatusReturnsError(t *testing.T) {
	t.Parallel()

	// 401 = trial token expired, 429 = rate-limited
	body := secondaryEnvelope(401, map[string]any{})

	if _, err := newTestSecondaryProvider().parseResponse(body); err == nil {
		t.Error("parseResponse of 401 envelope should error, got nil")
	}
}

func TestParseSecondaryResponse_MissingMessageFieldReturnsError(t *testing.T) {
	t.Parallel()

	// Some primary source error pages return JSON like `{"error":"..."}` 
	if _, err := newTestSecondaryProvider().parseResponse([]byte(`{"error":"bad"}`)); err == nil {
		t.Error("parseResponse without message field should error, got nil")
	}
}

func TestParseSecondaryResponse_EmptyBodyReturnsError(t *testing.T) {
	t.Parallel()

	if _, err := newTestSecondaryProvider().parseResponse([]byte("")); err == nil {
		t.Error("parseResponse of empty body should error, got nil")
	}
}

func TestParseSecondaryResponse_MalformedJSONReturnsError(t *testing.T) {
	t.Parallel()

	if _, err := newTestSecondaryProvider().parseResponse([]byte(`not json`)); err == nil {
		t.Error("parseResponse of malformed JSON should error, got nil")
	}
}

// handles primary source's variable body shape
func TestParseSecondarySubtitles_HappyPathDecodesSubtitleArray(t *testing.T) {
	t.Parallel()

	raw, _ := json.Marshal(secondarySubtitleCall(
		`[{"text":"L0","time":{"total":0}},{"text":"L1","time":{"total":17.33}}]`,
	))

	result, err := newTestSecondaryProvider().parseSubtitles(raw)
	if err != nil {
		t.Fatalf("parseSubtitles: %v", err)
	}
	if got, want := result.SyncType, "LINE_SYNCED"; got != want {
		t.Errorf("SyncType: got %q want %q", got, want)
	}
	if len(result.Lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(result.Lines))
	}
	// 17.33s × 1000 = 17330ms. int cast 
	if got, want := result.Lines[1].StartTimeMs, "17330"; got != want {
		t.Errorf("Lines[1].StartTimeMs: got %q want %q (time.total × 1000)", got, want)
	}
}

func TestParseSecondarySubtitles_BodyIsArraySentinel(t *testing.T) {
	t.Parallel()

	// regression, instead of object, two-step decode catches it
	raw := []byte(`{"message":{"header":{"status_code":200},"body":[]}}`)

	if _, err := newTestSecondaryProvider().parseSubtitles(raw); err == nil {
		t.Error("body=[] should error with 'no synced subtitles', got nil")
	}
}

func TestParseSecondarySubtitles_BodyIsNullSentinel(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"message":{"header":{"status_code":200},"body":null}}`)

	if _, err := newTestSecondaryProvider().parseSubtitles(raw); err == nil {
		t.Error("body=null should error, got nil")
	}
}

func TestParseSecondarySubtitles_EmptySubtitleList(t *testing.T) {
	t.Parallel()

	// Body is the object shape but subtitle_list is empty
	raw := []byte(`{"message":{"header":{"status_code":200},"body":{"subtitle_list":[]}}}`)

	if _, err := newTestSecondaryProvider().parseSubtitles(raw); err == nil {
		t.Error("empty subtitle_list should error, got nil")
	}
}

func TestParseSecondarySubtitles_EmptySubtitleBody(t *testing.T) {
	t.Parallel()

	// subtitle_body is a string field, if it's empty, there's nothing so should error 
	raw, _ := json.Marshal(secondarySubtitleCall(""))

	if _, err := newTestSecondaryProvider().parseSubtitles(raw); err == nil {
		t.Error("empty subtitle_body should error, got nil")
	}
}

func TestParseSecondarySubtitles_MalformedSubtitleBodyJSON(t *testing.T) {
	t.Parallel()

	raw, _ := json.Marshal(secondarySubtitleCall("not [a valid] json array"))

	if _, err := newTestSecondaryProvider().parseSubtitles(raw); err == nil {
		t.Error("malformed subtitle_body JSON should error, got nil")
	}
}

func TestParseSecondarySubtitles_InnerNon200StatusReturnsError(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"message":{"header":{"status_code":404},"body":{}}}`)

	if _, err := newTestSecondaryProvider().parseSubtitles(raw); err == nil {
		t.Error("inner status 404 should error, got nil")
	}
}

func TestParseSecondarySubtitles_PreservesUnicodeInText(t *testing.T) {
	t.Parallel()

	// same UTF-8 round-trip contract as parseLRC
	raw, _ := json.Marshal(secondarySubtitleCall(
		`[{"text":"こんにちは ♪","time":{"total":0}}]`,
	))

	result, err := newTestSecondaryProvider().parseSubtitles(raw)
	if err != nil {
		t.Fatalf("parseSubtitles: %v", err)
	}
	if got, want := result.Lines[0].Words, "こんにちは ♪"; got != want {
		t.Errorf("Words: got %q want %q", got, want)
	}
}

func TestParseSecondarySubtitles_FloatToMillisecondTruncation(t *testing.T) {
	t.Parallel()

	// int() truncates, not rounds. 0.999s = 999ms
	raw, _ := json.Marshal(secondarySubtitleCall(
		`[{"text":"L0","time":{"total":0.999}}]`,
	))

	result, err := newTestSecondaryProvider().parseSubtitles(raw)
	if err != nil {
		t.Fatalf("parseSubtitles: %v", err)
	}
	if got, want := result.Lines[0].StartTimeMs, "999"; got != want {
		t.Errorf("StartTimeMs: got %q want %q", got, want)
	}
}

// UNSYNCED fallback path
func TestParseSecondaryPlain_HappyPathYieldsUnsynced(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"message": {
			"header": {"status_code": 200},
			"body": {
				"lyrics": {
					"lyrics_body": "Verse one\nVerse two\nVerse three"
				}
			}
		}
	}`)

	result, err := newTestSecondaryProvider().parsePlain(raw)
	if err != nil {
		t.Fatalf("parsePlain: %v", err)
	}
	if got, want := result.SyncType, "UNSYNCED"; got != want {
		t.Errorf("SyncType: got %q want %q", got, want)
	}
	if len(result.Lines) != 3 {
		t.Errorf("expected 3 lines, got %d", len(result.Lines))
	}
}

func TestParseSecondaryPlain_Non200StatusReturnsError(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"message":{"header":{"status_code":404},"body":{"lyrics":{"lyrics_body":"x"}}}}`)

	if _, err := newTestSecondaryProvider().parsePlain(raw); err == nil {
		t.Error("non-200 status should error, got nil")
	}
}

func TestParseSecondaryPlain_EmptyBodyReturnsError(t *testing.T) {
	t.Parallel()

	// 200 status but no lyrics_body = "we have a record but no text" should error so we can try LRCLIB
	raw := []byte(`{"message":{"header":{"status_code":200},"body":{"lyrics":{"lyrics_body":""}}}}`)

	if _, err := newTestSecondaryProvider().parsePlain(raw); err == nil {
		t.Error("empty lyrics_body should error, got nil")
	}
}

func TestParseSecondaryPlain_MalformedJSONReturnsError(t *testing.T) {
	t.Parallel()

	if _, err := newTestSecondaryProvider().parsePlain([]byte("not json")); err == nil {
		t.Error("malformed JSON should error, got nil")
	}
}

// the primary source 
func TestParsePrimary_MapsSyncedLinesAndKeepsMsStrings(t *testing.T) {
	t.Parallel()

	// real shape from the primary source
	body := []byte(`{
		"lyrics": {
			"syncType": "LINE_SYNCED",
			"lines": [
				{"startTimeMs": "22290", "words": "first line", "syllables": [], "endTimeMs": "0", "transliteratedWords": ""},
				{"startTimeMs": "27830", "words": "second line", "syllables": [], "endTimeMs": "0", "transliteratedWords": ""},
				{"startTimeMs": "33680", "words": "", "syllables": [], "endTimeMs": "0", "transliteratedWords": ""}
			],
			"language": "en"
		}
	}`)

	result, err := parsePrimary(body)
	if err != nil {
		t.Fatalf("parsePrimary: %v", err)
	}
	if result.SyncType != "LINE_SYNCED" {
		t.Errorf("SyncType: got %q want LINE_SYNCED", result.SyncType)
	}
	if len(result.Lines) != 3 {
		t.Fatalf("lines: got %d want 3", len(result.Lines))
	}
	if result.Lines[0].StartTimeMs != "22290" || result.Lines[0].Words != "first line" {
		t.Errorf("line0: got %+v", result.Lines[0])
	}
	// blank gap lines are preserved
	if result.Lines[2].Words != "" || result.Lines[2].StartTimeMs != "33680" {
		t.Errorf("line2 (blank gap): got %+v", result.Lines[2])
	}
}

func TestParsePrimary_DefaultsEmptySyncTypeToUnsynced(t *testing.T) {
	t.Parallel()

	body := []byte(`{"lyrics": {"lines": [{"startTimeMs": "0", "words": "x"}]}}`)
	result, err := parsePrimary(body)
	if err != nil {
		t.Fatalf("parsePrimary: %v", err)
	}
	if result.SyncType != "UNSYNCED" {
		t.Errorf("SyncType: got %q want UNSYNCED", result.SyncType)
	}
}

func TestParsePrimary_NoLinesIsError(t *testing.T) {
	t.Parallel()

	if _, err := parsePrimary([]byte(`{"lyrics": {"syncType": "LINE_SYNCED", "lines": []}}`)); err == nil {
		t.Error("expected error for empty lines, got nil")
	}
}
