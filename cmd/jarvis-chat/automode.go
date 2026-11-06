// The owner's control surface for NoBrakes (task 3/6): the !automode
// on / off Discord command sets or clears the auto_mode flag in Redis that
// gates autonomous nightly job execution (nobrakes.AutoEnqueue reads it).
//
// It is owner-only by construction: the chat layer only fires the OnMessage
// hook for the owner DM (internal/chat/discord.go), so no non-owner message
// can ever reach handleAutoMode — the same guarantee that protects !pause
// and !restart.
package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/nobrakes"
)

// cutAutoMode splits an !automode command out of a chat message.
//
//	isCommand is true when the message starts with "!automode" followed by a
//	word boundary, so "!automodex" is not a command and falls through to the
//	brain as an ordinary message.
//
//	valid is true only when the argument is exactly one token, "on" or "off"
//	(case-insensitive). A bare "!automode" or a bad argument is still
//	isCommand (the owner clearly meant the switch) but valid is false, so the
//	handler answers with usage instead of sending it to the brain. on carries
//	the parsed value and is only meaningful when valid is true.
func cutAutoMode(content string) (on, valid, isCommand bool) {
	rest := strings.TrimPrefix(content, "!automode")
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

// handleAutoMode executes the !automode command: it dials the NoBrakes flag
// store, writes the requested state, reads the value back, and confirms the
// stored result to the owner. Redis failures never leave the owner guessing —
// each replies with a short explanation and leaves the flag unchanged.
func handleAutoMode(d sender, addr, content string) {
	on, valid, isCommand := cutAutoMode(content)
	if !isCommand {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if !valid {
		_ = d.Notify(ctx, "Usage: !automode on   or   !automode off")
		return
	}
	if addr == "off" || addr == "none" {
		_ = d.Notify(ctx, "NoBrakes is disabled here (JARVIS_REDIS is off), so there is no auto_mode flag to set.")
		return
	}

	flags, err := nobrakes.New(addr)
	if err != nil {
		log.Printf("jarvis-chat: !automode: redis %s unreachable: %v", addr, err)
		_ = d.Notify(ctx, "I can't reach the NoBrakes flag store (Redis), so auto_mode is unchanged.")
		return
	}
	defer flags.Close()

	if err := flags.SetAutoMode(ctx, on); err != nil {
		log.Printf("jarvis-chat: !automode: set: %v", err)
		_ = d.Notify(ctx, "I could not set auto_mode (Redis rejected the write); it is unchanged.")
		return
	}

	// Read the value back so the confirmation reflects what is actually
	// stored; a missing or corrupt value reads as "off" (the safe default).
	onStored := on
	if stored, err := flags.GetAutoMode(ctx); err == nil {
		onStored = stored == nobrakes.AutoModeOn
	} else {
		log.Printf("jarvis-chat: !automode: read-back: %v", err)
	}

	reply := nobrakes.AutoModeReply(onStored)
	if onStored && !nobrakes.UnitActive(nobrakes.LoopUnit) {
		// The nightly gate runs at the top of every agent-run loop
		// iteration: with the loop stopped the switch changes nothing
		// until the loop is up. Say so — but do NOT start the loop, that
		// would put it in supervised mode and it would ask on Discord who
		// runs the next job, which the owner did not ask for.
		reply += " Note: the agent-run loop is stopped, so nothing will run until it is started."
	}
	_ = d.Notify(ctx, reply)
}
