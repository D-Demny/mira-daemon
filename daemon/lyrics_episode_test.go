package daemon

import (
	"errors"
	"testing"
)

const sampleEpisodeText = `{
  "version": "1.0",
  "language": "en-us",
  "section": [
    {
      "startMs": 0,
      "fallback": { "sentence": { "startMs": 0, "text": "Music playing" } },
      "musicClosedCaption": { "endMs": 10559, "text": "Music playing" }
    },
    { "startMs": 12160, "title": { "title": "Speaker 3" } },
    {
      "startMs": 12160,
      "text": { "sentence": { "startMs": 12160, "text": "Gentlemen, we're live." } }
    },
    {
      "startMs": 14360,
      "text": { "sentence": { "startMs": 14360, "text": "What's happening?" } }
    }
  ]
}`

func TestParseEpisodeText_MapsSentencesTitlesAndFallback(t *testing.T) {
	res, err := parseEpisodeText([]byte(sampleEpisodeText))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.SyncType != "LINE_SYNCED" {
		t.Fatalf("expected LINE_SYNCED, got %q", res.SyncType)
	}

	// fallback caption + 2 sentences
	want := []LyricsLine{
		{StartTimeMs: "0", Words: "Music playing"},
		{StartTimeMs: "12160", Words: "Speaker 3: Gentlemen, we're live."},
		{StartTimeMs: "14360", Words: "What's happening?"},
	}
	if len(res.Lines) != len(want) {
		t.Fatalf("expected %d lines, got %d: %+v", len(want), len(res.Lines), res.Lines)
	}
	for i, w := range want {
		got := res.Lines[i]
		if got.StartTimeMs != w.StartTimeMs || got.Words != w.Words || len(got.Syllables) != 0 {
			t.Errorf("line %d: got %+v, want %+v", i, got, w)
		}
	}
}

func TestParseEpisodeText_EmptySectionsReturnsNoLyrics(t *testing.T) {
	_, err := parseEpisodeText([]byte(`{"version":"1.0","section":[]}`))
	if !errors.Is(err, ErrNoLyrics) {
		t.Fatalf("expected ErrNoLyrics, got %v", err)
	}
}

func TestParseEpisodeText_InvalidJSONErrors(t *testing.T) {
	if _, err := parseEpisodeText([]byte(`not json`)); err == nil {
		t.Fatal("expected error on invalid JSON")
	}
}
