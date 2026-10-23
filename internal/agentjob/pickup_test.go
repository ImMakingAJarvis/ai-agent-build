package agentjob

import (
	"context"
	"testing"
)

func TestPickupCalledAfterClaim(t *testing.T) {
	t.Setenv("JARVIS_DATA_DIR", t.TempDir())
	s := newStore(t)
	ingest(t, s, Spec{ID: "d1", Title: "D", DiscordChannelID: "c", DiscordMessageID: "m"})
	var seen Job
	r := runner(t, s, `echo ok`)
	r.Pickup = func(j Job) { seen = j }
	if _, err := r.Run(context.Background(), "d1", "r1", "local", ""); err != nil {
		t.Fatal(err)
	}
	if seen.ID != "d1" || seen.DiscordMessageID != "m" {
		t.Fatalf("pickup got %+v", seen.Spec)
	}
}

func TestDefaultPickupSkipsWithoutDiscord(t *testing.T) {
	// Must not panic or exec when IDs are empty.
	defaultDiscordPickup(Job{})
}
