package agentjob

import (
	"os"
	"os/exec"
)

// defaultDiscordPickup reacts to the Discord message that scheduled this
// job. Reaction failure is ignored so the coding round still runs.
func defaultDiscordPickup(j Job) {
	if j.DiscordChannelID == "" || j.DiscordMessageID == "" {
		return
	}
	cmd := exec.Command("jarvis-discord", "parrot", j.DiscordChannelID, j.DiscordMessageID)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}
