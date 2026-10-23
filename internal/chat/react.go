package chat

import (
	"context"
	"strings"

	"github.com/bwmarrin/discordgo"
)

var parrotNames = []string{
	"rainbow_parrot",
	"party_parrot",
	"partyparrot",
	"rainbowparrot",
}

const unicodeParrot = "🦜"

// React adds an emoji reaction. emoji is a unicode glyph or name:id
// (a:name:id for animated custom emoji).
func (d *Discord) React(ctx context.Context, channelID, messageID, emoji string) error {
	return d.s.MessageReactionAdd(channelID, messageID, emoji, discordgo.WithContext(ctx))
}

// ReactRainbowParrot prefers the guild's :rainbow_parrot: / party parrot
// custom emoji, then falls back to the unicode parrot. Callers should
// ignore the error so a missing Add Reactions permission does not fail work.
func (d *Discord) ReactRainbowParrot(ctx context.Context, channelID, messageID string) error {
	emoji := d.lookupPartyParrot()
	if emoji == "" {
		emoji = unicodeParrot
	}
	return d.React(ctx, channelID, messageID, emoji)
}

func (d *Discord) lookupPartyParrot() string {
	guilds, err := d.s.UserGuilds(20, "", "", false)
	if err != nil {
		return ""
	}
	var names []emojiRef
	for _, g := range guilds {
		emojis, err := d.s.GuildEmojis(g.ID)
		if err != nil {
			continue
		}
		for _, e := range emojis {
			names = append(names, emojiRef{Name: e.Name, ID: e.ID, Animated: e.Animated})
		}
	}
	return pickPartyParrot(names)
}

type emojiRef struct {
	Name     string
	ID       string
	Animated bool
}

func pickPartyParrot(emojis []emojiRef) string {
	byName := map[string]emojiRef{}
	for _, e := range emojis {
		byName[strings.ToLower(e.Name)] = e
	}
	for _, want := range parrotNames {
		if e, ok := byName[want]; ok {
			if e.Animated {
				return "a:" + e.Name + ":" + e.ID
			}
			return e.Name + ":" + e.ID
		}
	}
	return ""
}
