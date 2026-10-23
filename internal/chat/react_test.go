package chat

import "testing"

func TestPickPartyParrotPrefersRainbow(t *testing.T) {
	got := pickPartyParrot([]emojiRef{
		{Name: "party_parrot", ID: "2", Animated: true},
		{Name: "rainbow_parrot", ID: "1", Animated: true},
	})
	if got != "a:rainbow_parrot:1" {
		t.Fatalf("got %q", got)
	}
}

func TestPickPartyParrotFallbackParty(t *testing.T) {
	got := pickPartyParrot([]emojiRef{
		{Name: "Party_Parrot", ID: "9", Animated: false},
	})
	if got != "party_parrot:9" && got != "Party_Parrot:9" {
		t.Fatalf("got %q", got)
	}
}

func TestPickPartyParrotNone(t *testing.T) {
	if got := pickPartyParrot(nil); got != "" {
		t.Fatalf("got %q", got)
	}
}
