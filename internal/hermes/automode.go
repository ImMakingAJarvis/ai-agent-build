// The brain's NoBrakes control surface: the set_automode and set_fullauto
// actions flip the Redis flags directly, without scheduling a job.
//
// They exist because the owner asks for mode changes in natural language
// ("turn on full auto") and that must never be routed to enqueue_job —
// flipping a flag is not a job. The same switches are available as the
// Discord !automode / !fullauto commands and the jarvis-jobs CLI; every
// surface uses the same shared confirmations (nobrakes.AutoModeReply /
// FullAutoReply), so the owner hears the same words no matter how they
// flipped the switch.
package hermes

import (
	"context"
	"fmt"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/nobrakes"
)

// modeActions returns the two NoBrakes switch actions. Both require full
// access (they enable autonomous execution) and both fail soft: a Redis
// problem returns an owner-facing explanation instead of an error — the
// same behaviour as the !automode / !fullauto commands.
func modeActions() []Action {
	return []Action{
		{
			Key:                "set_automode",
			Description:        "Turn the NoBrakes auto_mode switch (autonomous nightly job runs) on or off directly in Redis. Parameter: mode = \"on\" or \"off\". Never schedules a job — use this, not enqueue_job, when the owner asks to change the mode.",
			RequiresFullAccess: true,
			Run: func(ctx context.Context, params map[string]string) (string, error) {
				return runSetAutoMode(ctx, params)
			},
		},
		{
			Key:                "set_fullauto",
			Description:        "Turn the NoBrakes full auto switch (the agent-run loop runs every ready job immediately, at any time of day) on or off directly in Redis. Parameter: mode = \"on\" or \"off\". Turning it on also starts the agent-run loop if it is stopped. Never schedules a job — use this, not enqueue_job, when the owner asks to change the mode.",
			RequiresFullAccess: true,
			Run: func(ctx context.Context, params map[string]string) (string, error) {
				return runSetFullAuto(ctx, params)
			},
		},
	}
}

// parseMode extracts the requested switch state from the action params. It
// accepts the words the classifier is likely to use for on/off; anything
// else is an error so a misclassification is visible rather than silently
// guessed.
func parseMode(params map[string]string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(params["mode"])) {
	case "on", "true", "enable", "enabled", "1":
		return true, nil
	case "off", "false", "disable", "disabled", "0":
		return false, nil
	case "":
		return false, fmt.Errorf("mode parameter is required (\"on\" or \"off\")")
	default:
		return false, fmt.Errorf("invalid mode %q (want \"on\" or \"off\")", params["mode"])
	}
}

// redisDisabled reports whether the NoBrakes flag store is disabled in this
// deployment ($JARVIS_REDIS is "off"/"none") — there is no Redis flag to set.
func redisDisabled() bool {
	addr := agentjob.StreamAddr()
	return addr == "off" || addr == "none"
}

// runSetAutoMode flips the auto_mode switch: it dials the flag store, writes
// the requested state, reads it back, and returns the shared confirmation
// for the stored value.
func runSetAutoMode(ctx context.Context, params map[string]string) (string, error) {
	on, err := parseMode(params)
	if err != nil {
		return "", err
	}
	if redisDisabled() {
		return "NoBrakes is disabled here (JARVIS_REDIS is off), so there is no auto_mode flag to set.", nil
	}
	flags, err := nobrakes.New(agentjob.StreamAddr())
	if err != nil {
		return "I can't reach the NoBrakes flag store (Redis), so auto_mode is unchanged.", nil
	}
	defer flags.Close()

	if err := flags.SetAutoMode(ctx, on); err != nil {
		return "I could not set auto_mode (Redis rejected the write); it is unchanged.", nil
	}
	onStored := on
	if stored, err := flags.GetAutoMode(ctx); err == nil {
		onStored = stored == nobrakes.AutoModeOn
	}
	return nobrakes.AutoModeReply(onStored), nil
}

// runSetFullAuto flips the full_auto switch. When the switch ends up on, it
// also wakes the agent-run loop if it is stopped, so full auto takes effect
// immediately instead of waiting for someone to start the loop by hand —
// the owner asked for the mode to be live, not parked. The wake note (empty
// when the loop was already up) is appended to the confirmation; it never
// blocks the confirmation.
func runSetFullAuto(ctx context.Context, params map[string]string) (string, error) {
	on, err := parseMode(params)
	if err != nil {
		return "", err
	}
	if redisDisabled() {
		return "NoBrakes is disabled here (JARVIS_REDIS is off), so there is no full-auto flag to set.", nil
	}
	flags, err := nobrakes.New(agentjob.StreamAddr())
	if err != nil {
		return "I can't reach the NoBrakes flag store (Redis), so full auto is unchanged.", nil
	}
	defer flags.Close()

	if err := flags.SetFullAuto(ctx, on); err != nil {
		return "I could not set full auto (Redis rejected the write); it is unchanged.", nil
	}
	onStored := on
	if stored, err := flags.GetFullAuto(ctx); err == nil {
		onStored = stored == nobrakes.AutoModeOn
	}

	reply := nobrakes.FullAutoReply(onStored)
	if onStored {
		if note := nobrakes.WakeLoop(ctx, nobrakes.LoopUnit); note != "" {
			reply += " " + note
		}
	}
	return reply, nil
}
