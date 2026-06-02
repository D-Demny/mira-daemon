package daemon

import (
	"testing"

	metadatapb "github.com/devgianlu/go-librespot/proto/spotify/metadata"
)

// getBestImageIdForSize picks album art by size

func makeImage(size metadatapb.Image_Size, id byte) *metadatapb.Image {
	return &metadatapb.Image{
		Size:   &size,
		FileId: []byte{id},
	}
}

func makeUnsizedImage(id byte) *metadatapb.Image {
	return &metadatapb.Image{FileId: []byte{id}}
}

func TestGetBestImageIdForSize_EmptyInputReturnsNil(t *testing.T) {
	t.Parallel()

	if got := getBestImageIdForSize(nil, "LARGE"); got != nil {
		t.Errorf("nil input: got %v want nil", got)
	}
	if got := getBestImageIdForSize([]*metadatapb.Image{}, "LARGE"); got != nil {
		t.Errorf("empty input: got %v want nil", got)
	}
}

func TestGetBestImageIdForSize_ExactMatchEarlyReturns(t *testing.T) {
	t.Parallel()

	images := []*metadatapb.Image{
		makeImage(metadatapb.Image_SMALL, 1),
		makeImage(metadatapb.Image_LARGE, 2),
		makeImage(metadatapb.Image_XLARGE, 3),
	}

	got := getBestImageIdForSize(images, "LARGE")
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("exact LARGE match should return FileId 2, got %v", got)
	}
}

func TestGetBestImageIdForSize_ChoosesClosestWhenNoExactMatch(t *testing.T) {
	t.Parallel()

	// LARGE=2 requested, SMALL=1 and XLARGE=3 both have dist=1
	images := []*metadatapb.Image{
		makeImage(metadatapb.Image_SMALL, 1),
		makeImage(metadatapb.Image_XLARGE, 3),
	}

	got := getBestImageIdForSize(images, "LARGE")
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("tied distance with SMALL first should pick SMALL (FileId 1), got %v", got)
	}

	imagesReversed := []*metadatapb.Image{
		makeImage(metadatapb.Image_XLARGE, 3),
		makeImage(metadatapb.Image_SMALL, 1),
	}
	got = getBestImageIdForSize(imagesReversed, "LARGE")
	if len(got) != 1 || got[0] != 3 {
		t.Errorf("tied distance with XLARGE first should pick XLARGE (FileId 3), got %v", got)
	}
}

func TestGetBestImageIdForSize_PicksUniqueClosest(t *testing.T) {
	t.Parallel()

	// XLARGE requested, LARGE wins over SMALL by distance, regardless of order
	for _, ordering := range []struct {
		name   string
		images []*metadatapb.Image
	}{
		{"large_first", []*metadatapb.Image{
			makeImage(metadatapb.Image_LARGE, 2),
			makeImage(metadatapb.Image_SMALL, 1),
		}},
		{"small_first", []*metadatapb.Image{
			makeImage(metadatapb.Image_SMALL, 1),
			makeImage(metadatapb.Image_LARGE, 2),
		}},
	} {
		t.Run(ordering.name, func(t *testing.T) {
			got := getBestImageIdForSize(ordering.images, "XLARGE")
			if len(got) != 1 || got[0] != 2 {
				t.Errorf("expected LARGE (FileId 2) closest to XLARGE, got %v", got)
			}
		})
	}
}

func TestGetBestImageIdForSize_SkipsUnsizedImagesDuringRanking(t *testing.T) {
	t.Parallel()

	images := []*metadatapb.Image{
		makeImage(metadatapb.Image_SMALL, 1),
		makeUnsizedImage(99),
		makeImage(metadatapb.Image_LARGE, 2),
	}

	got := getBestImageIdForSize(images, "LARGE")
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("unsized images should be skipped, exact match wins; got %v", got)
	}
}

func TestGetBestImageIdForSize_AllUnsizedFallsBackToFirst(t *testing.T) {
	t.Parallel()

	// all-unsized fallback to images[0].FileId
	images := []*metadatapb.Image{
		makeUnsizedImage(7),
		makeUnsizedImage(8),
		makeUnsizedImage(9),
	}

	got := getBestImageIdForSize(images, "LARGE")
	if len(got) != 1 || got[0] != 7 {
		t.Errorf("all-unsized fallback should return images[0].FileId (7), got %v", got)
	}
}
