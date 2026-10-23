// enqueue publishes a test job to the RabbitMQ task queue.
//
// Usage:
//
//	enqueue --type=voice --id=test-001 --prompt="Say hello"
//	enqueue --type=fix --id=test-002 --prompt="Fix the login bug"
//	enqueue --type=coding --id=test-003 --prompt="Write a Go hello world"
//	enqueue --type=background --id=test-004 --prompt="Benchmark the model"
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/queue"
)

func main() {
	jobType := flag.String("type", "background", "job type: voice, fix, coding, background")
	jobID := flag.String("id", fmt.Sprintf("cli-%d", time.Now().Unix()), "unique job id")
	prompt := flag.String("prompt", "", "prompt or description for the job")
	forceAPI := flag.Bool("force-api", false, "force dispatch to Claude API")
	modelOverride := flag.String("model-override", "", "override model selection")
	effort := flag.String("effort", queue.DefaultEffort,
		"reasoning effort: low, medium, high, xhigh, max")
	flag.Parse()

	if *prompt == "" {
		log.Fatal("--prompt is required")
	}

	msg := queue.Message{
		ID:        *jobID,
		Type:      *jobType,
		Priority:  1,
		CreatedAt: time.Now().UTC(),
		Source:    "cli",
		Payload: map[string]string{
			"prompt": *prompt,
		},
		ForceAPI:        *forceAPI,
		ModelOverride:   *modelOverride,
		ReasoningEffort: *effort,
	}

	// Set priority based on type.
	switch *jobType {
	case "voice":
		msg.Priority = 10
	case "fix":
		msg.Priority = 7
	case "coding":
		msg.Priority = 5
	default:
		msg.Priority = 1
	}

	conn, err := queue.NewConn()
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	routingKey := *jobType + ".default"
	if err := conn.Publish(msg, routingKey); err != nil {
		log.Fatalf("publish: %v", err)
	}

	fmt.Printf("Enqueued job %s → %s queue (routing key: %s)\n", msg.ID, *jobType, routingKey)
	os.Exit(0)
}
