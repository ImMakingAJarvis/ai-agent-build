// The owner's control surface for the NoBrakes keep-going mode: the
// !keepgoing on / off Discord command sets or clears the keep_going flag
// in Redis. When it is "on", a failed auto-executed job does NOT pause
// autonomous execution: the owner is still notified, auto_mode/full_auto
// stay as they are, and the agent-run loop moves on to the next ready job.
// When "off" (default), an auto failure trips autopause as usual.
//
// Like the other switches it is the operator's manual override and works
// at any time, including mid-round. Unlike !fullauto it never wakes the
// agent-run loop: keep_going is a modifier on the failure path, not an
// execution switch — it starts nothing by itself.
//
// It is owner-only by construction: the chat layer only fires the OnMessage
// hook for the owner DM (internal/chat/discord.go), so no non-owner message
// can ever reach handleKeepGoing — the same guarantee that protects !pause,
// !restart, !automode and !fullauto.
package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/nobrakes"
)

// cutKeepGoing splits a !keepgoing command out of a chat message.
//
//	isCommand is true when the message starts with "!keepgoing" followed by a
//	word boundary, so "!keepgoingx" is not a command and falls through to the
//	brain as an ordinary message.
//
//	valid is true only when the argument is exactly one token, "on" or "off"
//	(case-insensitive). A bare "!keepgoing" or a bad argument is still
//	isCommand (the owner clearly meant the switch) but valid is false, so the
//	handler answers with usage instead of sending it to the brain. on carries
//	the parsed value and is only meaningful when valid is true.
func cutKeepGoing(content string) (on, valid, isCommand bool) {
	rest := strings.TrimPrefix(content, "!keepgoing")
	if rest == content {
		return false, false, false
	}
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return false, false, false
	}
	fields := strings.Fields(rest)
	if len(fields) != 1 {
		return false, false, true // the command, but the argument is missing or not a single token
	}
	switch strings.ToLower(fields[0]) {
	case "on":
		return true, true, true
	case "off":
		return false, true, true
	default:
		return false, false, true
	}
}

// handleKeepGoing executes the !keepgoing command: it dials the NoBrakes
// flag store, writes the requested state, reads the value back, and
// confirms the stored result to the owner. Redis failures never leave the
// owner guessing — each replies with a short explanation and leaves the
// flag unchanged.
func handleKeepGoing(d sender, addr, content string) {
	on, valid, isCommand := cutKeepGoing(content)
	if !isCommand {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if !valid {
		_ = d.Notify(ctx, "Usage: !keepgoing on   or   !keepgoing off")
		return
	}
	if addr == "off" || addr == "none" {
		_ = d.Notify(ctx, "NoBrakes is disabled here (JARVIS_REDIS is off), so there is no keep-going flag to set.")
		return
	}

	flags, err := nobrakes.New(addr)
	if err != nil {
		log.Printf("jarvis-chat: !keepgoing: redis %s unreachable: %v", addr, err)
		_ = d.Notify(ctx, "I can't reach the NoBrakes flag store (Redis), so keep going is unchanged.")
		return
	}
	defer flags.Close()

	if err := flags.SetKeepGoing(ctx, on); err != nil {
		log.Printf("jarvis-chat: !keepgoing: set: %v", err)
		_ = d.Notify(ctx, "I could not set keep going (Redis rejected the write); it is unchanged.")
		return
	}

	// Read the value back so the confirmation reflects what is actually
	// stored; a missing or corrupt value reads as "off" (the safe default).
	onStored := on
	if stored, err := flags.GetKeepGoing(ctx); err == nil {
		onStored = stored == nobrakes.AutoModeOn
	} else {
		log.Printf("jarvis-chat: !keepgoing: read-back: %v", err)
	}

	_ = d.Notify(ctx, nobrakes.KeepGoingReply(onStored))
}
