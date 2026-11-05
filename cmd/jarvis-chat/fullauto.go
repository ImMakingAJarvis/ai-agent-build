// The owner's control surface for NoBrakes full auto mode: the !fullauto
// on / off Discord command sets or clears the full_auto flag in Redis.
// When it is "on", the agent-run loop runs the next ready job immediately
// — at any time of day, not just the nightly window — for as long as the
// queue has work, and idle-polls with an empty queue instead of asking
// who runs what. It is independent of auto_mode (the nightly batch) and
// can be combined with it.
//
// It is owner-only by construction: the chat layer only fires the OnMessage
// hook for the owner DM (internal/chat/discord.go), so no non-owner message
// can ever reach handleFullAuto — the same guarantee that protects !pause,
// !restart and !automode.
package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/nobrakes"
)

// cutFullAuto splits a !fullauto command out of a chat message. Both
// spellings of the switch are accepted: !fullauto (the short form the CLI
// and the checkpoint buttons use) and !full_auto (the underscored form
// the owner types).
//
//	isCommand is true when the message starts with one of the spellings
//	followed by a word boundary, so "!fullautox" is not a command and falls
//	through to the brain as an ordinary message.
//
//	valid is true only when the argument is exactly one token, "on" or "off"
//	(case-insensitive). A bare "!fullauto" or a bad argument is still
//	isCommand (the owner clearly meant the switch) but valid is false, so the
//	handler answers with usage instead of sending it to the brain. on carries
//	the parsed value and is only meaningful when valid is true.
func cutFullAuto(content string) (on, valid, isCommand bool) {
	rest, ok := cutCmdPrefix(content, "!fullauto", "!full_auto")
	if !ok {
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

// handleFullAuto executes the !fullauto command: it dials the NoBrakes flag
// store, writes the requested state, reads the value back, and confirms the
// stored result to the owner. Turning the switch ON also wakes the agent-run
// loop if it is stopped (nobrakes.WakeLoop), so full auto takes effect
// immediately instead of waiting for the loop to be started by hand — that
// is the point of a no-scheduling trigger. Redis failures never leave the
// owner guessing — each replies with a short explanation and leaves the flag
// unchanged.
func handleFullAuto(d sender, addr, content string) {
	on, valid, isCommand := cutFullAuto(content)
	if !isCommand {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if !valid {
		_ = d.Notify(ctx, "Usage: !full_auto on   or   !full_auto off   (the short form !fullauto works too)")
		return
	}
	if addr == "off" || addr == "none" {
		_ = d.Notify(ctx, "NoBrakes is disabled here (JARVIS_REDIS is off), so there is no full-auto flag to set.")
		return
	}

	flags, err := nobrakes.New(addr)
	if err != nil {
		log.Printf("jarvis-chat: !fullauto: redis %s unreachable: %v", addr, err)
		_ = d.Notify(ctx, "I can't reach the NoBrakes flag store (Redis), so full auto is unchanged.")
		return
	}
	defer flags.Close()

	if err := flags.SetFullAuto(ctx, on); err != nil {
		log.Printf("jarvis-chat: !fullauto: set: %v", err)
		_ = d.Notify(ctx, "I could not set full auto (Redis rejected the write); it is unchanged.")
		return
	}

	// Read the value back so the confirmation reflects what is actually
	// stored; a missing or corrupt value reads as "off" (the safe default).
	onStored := on
	if stored, err := flags.GetFullAuto(ctx); err == nil {
		onStored = stored == nobrakes.AutoModeOn
	} else {
		log.Printf("jarvis-chat: !fullauto: read-back: %v", err)
	}

	reply := nobrakes.FullAutoReply(onStored)
	if onStored {
		// Full auto is on and only means something while the agent-run
		// loop runs: if the loop is stopped, start it so the switch
		// takes effect now. The wake note (empty when the loop was
		// already up) is appended to the confirmation.
		if note := nobrakes.WakeLoop(ctx, nobrakes.LoopUnit); note != "" {
			reply += " " + note
		}
	}
	_ = d.Notify(ctx, reply)
}
