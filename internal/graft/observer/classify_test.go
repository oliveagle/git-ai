package observer

import (
	"encoding/json"
	"testing"
)

func TestChangelogEntryIsUnmarshalled(t *testing.T) {
	text := `{"sequence":11,"reference":"refs/heads/main","old_oid":"e336365b010b3af1ab00b0680ba4d5bafb3c4b75","new_oid":"3e306adec079d7fad84a8d71f32df0b914e956aa","operation":"land","actor":"shared","recorded_at":"2026-11-23T02:33:53.378624686Z","members":[{"original_commit":"3e306adec079d7fad84a8d71f32df0b914e956aa","commit":"3e306adec079d7fad84a8d71f32df0b914e956aa","actor":"oliveagle"}],"lands":[{"original":"3e306adec079d7fad84a8d71f32df0b914e956aa","landed":"3e306adec079d7fad84a8d71f32df0b914e956aa"}],"land_invocation":"995ee9e1729924302137545eba958f8f"}`
	var entry changelogEntry
	if err := json.Unmarshal([]byte(text), &entry); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	t.Logf("members=%d lands=%d invocation=%q", len(entry.Members), len(entry.Lands), entry.LandInvocation)
	if len(entry.Members) != 1 {
		t.Fatalf("members = %d, want 1", len(entry.Members))
	}
	if entry.LandInvocation == "" {
		t.Fatal("the land invocation is empty")
	}
}
