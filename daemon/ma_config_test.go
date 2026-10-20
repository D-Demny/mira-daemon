package daemon

import "testing"

// issue #134 (daemon part 1): ParseMaConfig / playbackSourceFromSettings
// over the UI settings blob. Mirrors the strictness of the HA and Pi
// profile parsers: any unusable blob means "backend disabled", never a
// parse error, and pre-#134 blobs stay byte-identical (issue #134
// contract).

func TestParseMaConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		blob    string
		wantNil bool
		wantURL string
		wantTok string
	}{
		{"empty blob", "", true, "", ""},
		{"malformed json", `{nope`, true, "", ""},
		{"missing ma key (old UI)", `{"volume": 40}`, true, "", ""},
		{"ma null", `{"ma": null}`, true, "", ""},
		{"ma not object", `{"ma": "oops"}`, true, "", ""},
		{"empty url disables", `{"ma": {"url": "", "token": "t"}}`, true, "", ""},
		{"whitespace-only url disables", `{"ma": {"url": "   ", "token": "t"}}`, true, "", ""},
		{"bare host:port canonicalized to http", `{"ma": {"url": "10.10.1.104:8095", "token": "tok"}}`, false, "http://10.10.1.104:8095", "tok"},
		{"https url kept", `{"ma": {"url": "https://ma.example.com", "token": "t2"}}`, false, "https://ma.example.com", "t2"},
		{"ws scheme mapped to http", `{"ma": {"url": "ws://10.10.1.104:8095", "token": "t3"}}`, false, "http://10.10.1.104:8095", "t3"},
		{"wss scheme mapped to https", `{"ma": {"url": "wss://ma.example.com", "token": "t4"}}`, false, "https://ma.example.com", "t4"},
		{"trailing slash stripped", `{"ma": {"url": "http://10.10.1.104:8095/", "token": "t5"}}`, false, "http://10.10.1.104:8095", "t5"},
		{"surrounding whitespace trimmed", `{"ma": {"url": "  http://ma.local:8095  ", "token": "t6"}}`, false, "http://ma.local:8095", "t6"},
		{"unknown scheme rejected", `{"ma": {"url": "ftp://ma.example.com", "token": "t7"}}`, true, "", ""},
		{"token verbatim with spaces", `{"ma": {"url": "http://h:1", "token": " tok-123 "}}`, false, "http://h:1", " tok-123 "},
		{"empty token allowed", `{"ma": {"url": "http://h:1", "token": ""}}`, false, "http://h:1", ""},
		{"unknown keys ignored", `{"ha": {"url": "x"}, "pi_profiles": {}, "ma": {"url": "http://h:2", "token": "t8"}}`, false, "http://h:2", "t8"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseMaConfig([]byte(tc.blob))
			if tc.wantNil {
				if got != nil {
					t.Fatalf("want nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("want config, got nil")
			}
			if got.URL != tc.wantURL {
				t.Errorf("url: got %q want %q", got.URL, tc.wantURL)
			}
			if got.Token != tc.wantTok {
				t.Errorf("token: got %q want %q", got.Token, tc.wantTok)
			}
		})
	}
}

func TestPlaybackSourceFromSettings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		blob string
		want string
	}{
		{"empty blob", "", ""},
		{"malformed", `{nope`, ""},
		{"absent key (old UI)", `{"volume": 40}`, ""},
		{"spotify explicit", `{"playbackSource": "spotify"}`, "spotify"},
		{"ma selected", `{"playbackSource": "ma", "ma": {"url": "http://h:1", "token": "t"}}`, "ma"},
		{"unknown value passed through (callers treat as spotify)", `{"playbackSource": "youtube"}`, "youtube"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := playbackSourceFromSettings([]byte(tc.blob)); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}
