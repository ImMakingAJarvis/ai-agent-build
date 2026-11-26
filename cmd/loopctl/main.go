// loopctl is the command-line client for the event-loop runtime
// (ELR-6 skeleton; M2.4 adds "config show", card ELR-23; M3.6a adds
// "run", card ELR-34).
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/ImMakingAJarvis/ai-agent-build/runtime/config"
	"github.com/ImMakingAJarvis/ai-agent-build/runtime/engine/llamacpp"
)

func main() {
	args := os.Args[1:]
	switch {
	case len(args) == 1 && args[0] == "version":
		fmt.Println("loopctl dev")
	case len(args) == 2 && args[0] == "config" && args[1] == "show":
		c, err := config.Load(configPath())
		if err != nil {
			fmt.Fprintln(os.Stderr, "loopctl:", err)
			os.Exit(1)
		}
		fmt.Print(c.YAML())
	case len(args) >= 1 && args[0] == "run":
		// The engine URL comes from the config; run() loads the config
		// again for the slots, timeouts and the rest.
		cfg, err := config.Load(configPath())
		if err != nil {
			fmt.Fprintln(os.Stderr, "loopctl:", err)
			os.Exit(1)
		}
		os.Exit(run(args, llamacpp.New(cfg.Engine.URL), os.Stdout))
	default:
		fmt.Fprintln(os.Stderr, `usage: loopctl version
       loopctl config show
       loopctl run [--slot N] [--workspace DIR] "<instructions>" ["<instructions>" ...]`)
		os.Exit(1)
	}
}

// configPath is the runtime config file: $LOOPD_CONFIG, defaulting to
// ~/.config/jarvis/loopd.yaml (M2.4, ELR-23).
func configPath() string {
	if p := os.Getenv("LOOPD_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".config/jarvis/loopd.yaml"
	}
	return filepath.Join(home, ".config", "jarvis", "loopd.yaml")
}
