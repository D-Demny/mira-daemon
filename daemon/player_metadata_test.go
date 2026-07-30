package daemon

import "testing"

// unofficial devices author their own ProvidedTrack metadata map and usually leave it empty or partial

func TestClusterToRemoteState_ImageKeyFallbacks(t *testing.T) {
	t.Parallel()

	c := baseCluster()
	md := c.PlayerState.Track.Metadata
	delete(md, "image_url")
	md["image_xlarge_url"] = "spotify:image:cafebabe"
	rs := clusterToRemoteState(c)
	if rs.TrackImageUrl != "https://i.scdn.co/image/cafebabe" {
		t.Errorf("xlarge fallback: got %q", rs.TrackImageUrl)
	}

	c.PlayerState.Track.Metadata = map[string]string{}
	rs = clusterToRemoteState(c)
	if rs.TrackImageUrl != "" || rs.TrackName != "" {
		t.Errorf("empty metadata: image=%q name=%q, want empty", rs.TrackImageUrl, rs.TrackName)
	}
	if rs.TrackUri != "spotify:track:abc123" {
		t.Errorf("uri must survive empty metadata, got %q", rs.TrackUri)
	}
}
