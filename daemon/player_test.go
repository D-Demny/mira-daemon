package daemon

import (
	"encoding/json"
	"testing"
)

// convertSpotifyImageUrl normalizes the image_url field to a usable https URL
func TestConvertSpotifyImageUrl(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			// standard shape from PlayerState.Track.Metadata["image_url"]
			name: "spotify_image_prefix",
			in:   "spotify:image:ab67616d00001e02deadbeef",
			want: "https://i.scdn.co/image/ab67616d00001e02deadbeef",
		},
		{
			// already-absolute https, pass through
			name: "already_https",
			in:   "https://i.scdn.co/image/abc",
			want: "https://i.scdn.co/image/abc",
		},
		{
			name: "already_http",
			in:   "http://example.com/img.png",
			want: "http://example.com/img.png",
		},
		{
			// default branch, prepend https:// so at least it's a valid URL
			name: "default_prepends_https",
			in:   "bare-string",
			want: "https://bare-string",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := convertSpotifyImageUrl(tt.in); got != tt.want {
				t.Errorf("convertSpotifyImageUrl(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDefaultDeviceIdFromSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   json.RawMessage
		want string
	}{
		{
			name: "nil returns empty",
			in:   nil,
			want: "",
		},
		{
			name: "empty object returns empty",
			in:   json.RawMessage(`{}`),
			want: "",
		},
		{
			name: "null value returns empty",
			in:   json.RawMessage(`{"default_device_id": null}`),
			want: "",
		},
		{
			name: "empty string returns empty",
			in:   json.RawMessage(`{"default_device_id": ""}`),
			want: "",
		},
		{
			name: "valid device id",
			in:   json.RawMessage(`{"default_device_id": "abc123-def456"}`),
			want: "abc123-def456",
		},
		{
			name: "other fields ignored",
			in:   json.RawMessage(`{"voiceMic": true, "default_device_id": "xyz789"}`),
			want: "xyz789",
		},
		{
			name: "invalid json returns empty",
			in:   json.RawMessage(`{invalid}`),
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := defaultDeviceIdFromSettings(tt.in); got != tt.want {
				t.Errorf("defaultDeviceIdFromSettings(%s) = %q, want %q", string(tt.in), got, tt.want)
			}
		})
	}
}
