package daemon

import "testing"

func TestRenderBacklightConf(t *testing.T) {
	tests := []struct {
		name   string
		blob   string
		want   string
		wantOk bool
	}{
		{
			name:   "all keys present",
			blob:   `{"v":1,"autoBrightness":true,"brightness":7,"showLyrics":true}`,
			want:   "AUTO=1\nBRIGHTNESS=7\n",
			wantOk: true,
		},
		{
			name:   "auto off",
			blob:   `{"autoBrightness":false,"brightness":3}`,
			want:   "AUTO=0\nBRIGHTNESS=3\n",
			wantOk: true,
		},
		{
			name:   "partial keys fall back to defaults",
			blob:   `{"brightness":4}`,
			want:   "AUTO=1\nBRIGHTNESS=4\n",
			wantOk: true,
		},
		{
			name:   "out-of-range values clamp",
			blob:   `{"autoBrightness":false,"brightness":99}`,
			want:   "AUTO=0\nBRIGHTNESS=10\n",
			wantOk: true,
		},
		{
			name:   "pre-brightness blob carries none of the keys",
			blob:   `{"v":1,"showLyrics":false,"volumeStepPct":2}`,
			wantOk: false,
		},
		{
			name:   "unparseable blob",
			blob:   `not json`,
			wantOk: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := renderBacklightConf([]byte(tt.blob))
			if ok != tt.wantOk {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOk)
			}
			if ok && got != tt.want {
				t.Fatalf("conf = %q, want %q", got, tt.want)
			}
		})
	}
}
