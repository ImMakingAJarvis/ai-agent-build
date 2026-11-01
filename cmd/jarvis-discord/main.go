// jarvis-discord lets scripts and the bootstrap agent reach the owner on
// Discord.
//
//	jarvis-discord notify <message>          send a message (stdin if no args)
//	jarvis-discord notify --item AAB-4 <msg> send the message with the Plane
//	                                       board link for the item appended
//	jarvis-discord ask [flags] <question>    send a question, print the answer
//
// ask flags:
//
//	-choices "yes,no"   show buttons; a typed reply is accepted too
//	-timeout 2h         give up after this long (exit 3); default waits forever
//
// notify flags:
//
//	-item <ref>  Plane work item (UUID or display id like AAB-43) whose
//	            board link is appended on its own line
//
// Config comes from DISCORD_BOT_TOKEN and DISCORD_OWNER_ID, or from the file
// named by JARVIS_DISCORD_ENV (default ~/.config/jarvis/discord.env) holding
// the same KEY=value lines.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/chat"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/plane"
)

const usage = `usage:
  jarvis-discord notify [--item AAB-43] <message>
  jarvis-discord ask [-choices a,b,c] [-timeout 2h] <question>
  jarvis-discord watch
  jarvis-discord parrot <channelID> <messageID>`

func main() {
	if len(os.Args) < 2 {
		fail(usage)
	}
	loadEnvFile()

	signalFile := ""
	if conf := os.Getenv("JARVIS_CONF"); conf != "" {
		signalFile = conf + "/pause-signal"
	}
	d, err := chat.NewDiscord(chat.Config{
		Token:      os.Getenv("DISCORD_BOT_TOKEN"),
		OwnerID:    os.Getenv("DISCORD_OWNER_ID"),
		SignalFile: signalFile,
	})
	if err != nil {
		fail(err.Error())
	}
	defer d.Close()

	ctx := context.Background()
	switch os.Args[1] {
	case "watch":
		// Keep the gateway open so onMessage fires for every incoming
		// DM. When there's no pending Ask, onMessage writes the signal
		// file to pause the running job.
		if err := d.Listen(); err != nil {
			fail(err.Error())
		}
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
		select {
		case <-sig:
		case <-time.After(24 * time.Hour):
		}

	case "parrot":
		if len(os.Args) != 4 {
			fail("usage: jarvis-discord parrot <channelID> <messageID>")
		}
		if err := d.ReactRainbowParrot(ctx, os.Args[2], os.Args[3]); err != nil {
			fail(err.Error())
		}

	case "notify":
		fs := flag.NewFlagSet("notify", flag.ExitOnError)
		item := fs.String("item", "", "Plane work item whose board link is appended")
		fs.Parse(os.Args[2:])
		text := message(fs.Args())
		if *item != "" {
			link, err := boardItemLink(ctx, *item)
			if err != nil {
				fail(err.Error())
			}
			text = text + "\n" + link
		}
		if err := d.Notify(ctx, text); err != nil {
			fail(err.Error())
		}

	case "ask":
		fs := flag.NewFlagSet("ask", flag.ExitOnError)
		choices := fs.String("choices", "", "comma-separated button labels")
		timeout := fs.Duration("timeout", 0, "give up after this long (0 = wait forever)")
		fs.Parse(os.Args[2:])

		if *timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, *timeout)
			defer cancel()
		}
		var opts []string
		if *choices != "" {
			opts = strings.Split(*choices, ",")
		}

		// Write marker so jarvis-chat skips messages while checkpoint is open.
		markerCleanup, err := writeAskOpenMarker()
		if err != nil {
			fail(err.Error())
		}

		// On signal, clean up marker and exit.
		if markerCleanup != nil {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
			go func() {
				<-sig
				markerCleanup()
				os.Exit(1)
			}()
		}

		answer, err := d.Ask(ctx, message(fs.Args()), opts)
		if markerCleanup != nil {
			markerCleanup()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			fmt.Fprintln(os.Stderr, "no answer before timeout")
			os.Exit(3)
		}
		if err != nil {
			fail(err.Error())
		}
		fmt.Println(answer)

	default:
		fail(usage)
	}
}

// boardItemLink resolves a Plane work item to its browser URL (P6: status
// messages carry the board link). Config comes from the PLANE_* environment
// or the daemon env file, like jarvis-board.
func boardItemLink(ctx context.Context, ref string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	c, err := plane.FromEnv()
	if err != nil {
		return "", err
	}
	project := strings.TrimSpace(os.Getenv("PLANE_PROJECT"))
	if project == "" {
		project = "AAB"
	}
	p, err := plane.ProjectByRef(ctx, c, project)
	if err != nil {
		return "", err
	}
	wi, err := plane.WorkItemByRef(ctx, c, p.ID, ref)
	if err != nil {
		return "", err
	}
	return c.ItemLink(p, wi), nil
}

// message joins args, or reads stdin when there are none.
func message(args []string) string {
	if len(args) > 0 {
		return strings.Join(args, " ")
	}
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		fail(err.Error())
	}
	return strings.TrimSpace(string(b))
}

// loadEnvFile fills in unset variables from the env file, if it exists.
func loadEnvFile() {
	path := os.Getenv("JARVIS_DISCORD_ENV")
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".config", "jarvis", "discord.env")
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || strings.HasPrefix(line, "#") || os.Getenv(k) != "" {
			continue
		}
		os.Setenv(k, strings.Trim(strings.TrimSpace(v), `"'`))
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "jarvis-discord:", msg)
	os.Exit(1)
}

// writeAskOpenMarker writes the ask-open marker file so jarvis-chat knows a
// checkpoint question is pending. Returns a cleanup function (or nil when
// JARVIS_CONF is not set).
func writeAskOpenMarker() (cleanup func(), err error) {
	conf := os.Getenv("JARVIS_CONF")
	if conf == "" {
		return nil, nil
	}
	path := filepath.Join(conf, "ask-open")
	if err := os.WriteFile(path, []byte("checkpoint question open"), 0o644); err != nil {
		return nil, err
	}
	return func() { os.Remove(path) }, nil
}
