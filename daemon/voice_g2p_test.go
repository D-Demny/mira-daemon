package daemon

import (
	"context"
	"testing"
)

func TestIpaManyOverrideDedup(t *testing.T) {
	g := newG2P("", "", "", "")
	calls := 0
	g.override = func(s string) string {
		calls++
		return "ipa:" + s
	}
	ctx := context.Background()
	res := g.ipaMany(ctx, []string{"Heartless", "heartless", "Vultures", ""})
	if calls != 2 {
		t.Errorf("override calls = %d, want 2 (deduped, empty skipped)", calls)
	}
	if res["heartless"] != "ipa:heartless" || res["vultures"] != "ipa:vultures" {
		t.Errorf("ipaMany result wrong: %+v", res)
	}
	g.ipaMany(ctx, []string{"Heartless", "Vultures"})
	if calls != 2 {
		t.Errorf("cache miss on second pass: calls = %d, want 2", calls)
	}
}

func TestIpaCachesAndMemoizes(t *testing.T) {
	g := newG2P("", "", "", "")
	calls := 0
	g.override = func(s string) string { calls++; return "x" + s }
	ctx := context.Background()
	_ = g.ipa(ctx, "Kanye West")
	_ = g.ipa(ctx, "kanye west")
	if calls != 1 {
		t.Errorf("ipa memoization failed: calls = %d, want 1", calls)
	}
}
