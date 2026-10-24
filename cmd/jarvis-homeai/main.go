// jarvis-homeai handles Home AI interactions: presence detection,
// voice announcements, and the HomeAI dashboard in Home Assistant.
//
// Commands:
//   jarvis-homeai announce "message"
//     Check the announcement switch and user presence, then send TTS.
//   jarvis-homeai presence
//     Check if the user is home (prints "home" or "not_home").
//   jarvis-homeai switch [on|off|get]
//     Manage the job-announcement toggle switch in Home Assistant.
//   jarvis-homeai players
//     List all media player entities.
//   jarvis-homeai tts "message"
//     Send TTS directly (bypassing switch and presence checks).
package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/notify"
)

const (
	// Switch entity for toggling job completion announcements.
	switchEntity = "input_boolean.job_announce"

	// Person entity for checking if the owner is home.
	// HA_PERSON_ENTITY can override the default.
	defaultPersonEntity = "person.owner"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: jarvis-homeai <command> [args...]\n")
		fmt.Fprintf(os.Stderr, "Commands: announce, presence, switch, players, tts\n")
		os.Exit(1)
	}

	ha := notify.NewClient()
	if !ha.Configured() {
		log.Printf("homeai: Home Assistant not configured (HA_TOKEN is empty); skipping")
		os.Exit(0)
	}

	cmd := os.Args[1]
	switch cmd {
	case "announce":
		if err := doAnnounce(ha); err != nil {
			log.Fatalf("homeai: announce: %v", err)
		}
	case "presence":
		if err := doPresence(ha); err != nil {
			log.Fatalf("homeai: presence: %v", err)
		}
	case "switch":
		if err := doSwitch(ha); err != nil {
			log.Fatalf("homeai: switch: %v", err)
		}
	case "players":
		if err := doPlayers(ha); err != nil {
			log.Fatalf("homeai: players: %v", err)
		}
	case "tts":
		if err := doTTS(ha); err != nil {
			log.Fatalf("homeai: tts: %v", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		os.Exit(1)
	}
}

// doAnnounce checks the switch and presence, then sends TTS if both allow it.
func doAnnounce(ha *notify.Client) error {
	// Check if the announcement switch is on.
	switchOn, err := entityIsOn(ha, switchEntity)
	if err != nil {
		log.Printf("homeai: could not read switch %s: %v (skipping announcement)", switchEntity, err)
		return nil
	}
	if !switchOn {
		log.Printf("homeai: announcement switch is off; skipping")
		return nil
	}

	// Check if the user is home.
	personEntity := os.Getenv("HA_PERSON_ENTITY")
	if personEntity == "" {
		personEntity = defaultPersonEntity
	}
	isHome, err := isPersonHome(ha, personEntity)
	if err != nil {
		log.Printf("homeai: could not check presence for %s: %v (skipping announcement)", personEntity, err)
		return nil
	}
	if !isHome {
		log.Printf("homeai: owner is not home; skipping announcement")
		return nil
	}

	// Send the announcement.
	msg := strings.Join(os.Args[2:], " ")
	if msg == "" {
		msg = "I have finished my work."
	}
	log.Printf("homeai: announcing to all speakers: %s", msg)
	return ha.TTSToAll(msg)
}

// doPresence checks if the user is home and prints the result.
func doPresence(ha *notify.Client) error {
	personEntity := os.Getenv("HA_PERSON_ENTITY")
	if personEntity == "" {
		personEntity = defaultPersonEntity
	}
	isHome, err := isPersonHome(ha, personEntity)
	if err != nil {
		return err
	}
	if isHome {
		fmt.Println("home")
	} else {
		fmt.Println("not_home")
	}
	return nil
}

// doSwitch manages the announcement toggle switch.
func doSwitch(ha *notify.Client) error {
	action := "get"
	if len(os.Args) >= 3 {
		action = strings.ToLower(os.Args[2])
	}

	switch action {
	case "on":
		return ha.CallService("input_boolean.turn_on", map[string]any{
			"entity_id": switchEntity,
		})
	case "off":
		return ha.CallService("input_boolean.turn_off", map[string]any{
			"entity_id": switchEntity,
		})
	case "get":
		entity, err := ha.GetState(switchEntity)
		if err != nil {
			return err
		}
		fmt.Println(entity.State)
		return nil
	default:
		return fmt.Errorf("unknown switch action %q; use on, off, or get", action)
	}
}

// doPlayers lists all media player entities.
func doPlayers(ha *notify.Client) error {
	players, err := ha.FindMediaPlayers()
	if err != nil {
		return err
	}
	for _, p := range players {
		fmt.Println(p)
	}
	return nil
}

// doTTS sends TTS directly without checks.
func doTTS(ha *notify.Client) error {
	msg := strings.Join(os.Args[2:], " ")
	if msg == "" {
		return fmt.Errorf("tts: message is required")
	}
	log.Printf("homeai: sending TTS (direct): %s", msg)
	return ha.TTSToAll(msg)
}

// entityIsOn checks if an entity's state is "on".
func entityIsOn(ha *notify.Client, entityID string) (bool, error) {
	entity, err := ha.GetState(entityID)
	if err != nil {
		return false, err
	}
	return entity.State == "on", nil
}

// isPersonHome checks if a person entity's state is "home".
func isPersonHome(ha *notify.Client, entityID string) (bool, error) {
	entity, err := ha.GetState(entityID)
	if err != nil {
		return false, err
	}
	return entity.State == "home", nil
}
