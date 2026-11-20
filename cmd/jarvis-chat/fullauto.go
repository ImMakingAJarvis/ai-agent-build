// The owner's control surface for NoBrakes full auto mode: the
// !fullauto on <reviewer> / off Discord command sets or clears the
// full_auto flag in Redis, and — on — stores the reviewer id full-auto
// jobs will review with in the companion key full_auto_reviewer (next to
// the flag, not overloading its on/off value). When it is "on", the
// agent-run loop runs the next ready job immediately — at any time of
// day, not just the nightly window — for as long as the queue has work,
// and idle-polls with an empty queue instead of asking who runs what. It
// is independent of auto_mode (the nightly batch) and can be combined
// with it.
//
// It is owner-only by construction: the chat layer only fires the OnMessage
// hook for the owner DM (internal/chat/discord.go), so no non-owner message
// can ever reach handleFullAuto — the same guarantee that protects !pause,
// !restart and !automode.
package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/nobrakes"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/reviewroute"
)

// cutFullAuto splits a !fullauto command out of a chat message. Both
// spellings of the switch are accepted: !fullauto (the short form the CLI
// and the checkpoint buttons use) and !full_auto (the underscored form
// the owner types).
//
// Grammar (PR review queues build, Job 5b — !fullauto on names the
// reviewer):
//
//	!fullauto on <reviewer>   on, reviewer is the id lowercased
//	!fullauto off             off, reviewer empty
//
//	isCommand is true when the message starts with one of the spellings
//	followed by a word boundary, so "!fullautox" is not a command and falls
//	through to the brain as an ordinary message.
//
//	valid is true only for exactly those two forms. A bare "!fullauto on"
//	without a reviewer is deliberately invalid (the breaking change in Job
//	5b): full auto without a named reviewer has no defined behaviour in
//	the review queues build, so the handler answers with usage instead of
//	flipping the flag. A bare "!fullauto" or a bad argument is still
//	isCommand (the owner clearly meant the switch) but valid is false. on
//	and reviewer are only meaningful when valid is true.
func cutFullAuto(content string) (on bool, reviewer string, valid, isCommand bool) {
	rest, ok := cutCmdPrefix(content, "!fullauto", "!full_auto")
	if !ok {
		return false, "", false, false
	}
	fields := strings.Fields(rest)
	if len(fields) == 1 && strings.EqualFold(fields[0], "off") {
		return false, "", true, true
	}
	if len(fields) == 2 && strings.EqualFold(fields[0], "on") {
		return true, strings.ToLower(fields[1]), true, true
	}
	return false, "", false, true // the command, but not in one of the two forms
}

// handleFullAuto executes the !fullauto command: it dials the NoBrakes flag
// store, writes the requested state (and the reviewer, when on), reads the
// values back, and confirms the stored result to the owner. A reviewer the
// review config does not know is rejected before anything is written — the
// reply says the name is unknown and full auto is not turned on. Turning
// the switch ON also wakes the agent-run loop if it is stopped
// (nobrakes.WakeLoop), so full auto takes effect immediately instead of
// waiting for the loop to be started by hand — that is the point of a
// no-scheduling trigger. Redis failures never leave the owner guessing —
// each replies with a short explanation and leaves the flag unchanged.
func handleFullAuto(d sender, addr, content string) {
	on, reviewer, valid, isCommand := cutFullAuto(content)
	if !isCommand {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if !valid {
		_ = d.Notify(ctx, "Usage: !full_auto on <reviewer>   or   !full_auto off   (the short form !fullauto works too)")
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

	// The reviewer is validated against the review config before anything
	// is written: a name the config does not know must not turn full auto
	// on.
	if on {
		cfg, err := loadReviewConfig()
		if err != nil {
			log.Printf("jarvis-chat: !fullauto: review config: %v", err)
			_ = d.Notify(ctx, "I can't read the review config, so I can't check that reviewer — full auto is unchanged.")
			return
		}
		if _, err := reviewroute.Queue(reviewer, cfg); err != nil {
			_ = d.Notify(ctx, fmt.Sprintf("I don't know that reviewer (%v) — full auto is unchanged.", err))
			return
		}
	}

	// The reviewer key first, then the flag: a stored reviewer with full
	// auto off is inert, but full auto on with no reviewer stored is not.
	// Off clears the stored reviewer too.
	reviewerToStore := ""
	if on {
		reviewerToStore = reviewer
	}
	if err := flags.SetFullAutoReviewer(ctx, reviewerToStore); err != nil {
		log.Printf("jarvis-chat: !fullauto: set reviewer: %v", err)
		_ = d.Notify(ctx, "I could not store the reviewer (Redis rejected the write); full auto is unchanged.")
		return
	}
	if err := flags.SetFullAuto(ctx, on); err != nil {
		log.Printf("jarvis-chat: !fullauto: set: %v", err)
		_ = d.Notify(ctx, "I could not set full auto (Redis rejected the write); it is unchanged.")
		return
	}

	// Read the values back so the confirmation reflects what is actually
	// stored; a missing or corrupt value reads as the safe default.
	onStored := on
	if stored, err := flags.GetFullAuto(ctx); err == nil {
		onStored = stored == nobrakes.AutoModeOn
	} else {
		log.Printf("jarvis-chat: !fullauto: read-back: %v", err)
	}
	reviewerStored := reviewer
	if r, err := flags.GetFullAutoReviewer(ctx); err == nil {
		reviewerStored = r
	} else {
		log.Printf("jarvis-chat: !fullauto: read-back reviewer: %v", err)
	}

	reply := nobrakes.FullAutoReply(onStored)
	if onStored && reviewerStored != "" {
		reply += fmt.Sprintf(" PRs it opens will be reviewed by %s.", reviewerStored)
	}
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
