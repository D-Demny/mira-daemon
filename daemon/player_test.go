package daemon

import "testing"

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
