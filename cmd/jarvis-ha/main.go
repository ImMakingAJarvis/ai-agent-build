// jarvis-ha publishes JARVIS headline status to Home Assistant as
// MQTT auto-discovered sensors (Phase 7): queue depth per queue,
// current job, loaded model, today's API spend, GPU temp/power.
//
// It expects an MQTT broker (RabbitMQ with the rabbitmq_mqtt plugin)
// and publishes discovery configs once, then state values on a fixed
// interval. Home Assistant's built-in MQTT integration picks the
// entities up automatically; see ha/README.md for the HA-side setup.
//
// Environment:
//
//	HA_MQTT_URL          broker URL          (default mqtt://localhost:1883)
//	HA_MQTT_USER/PASS    broker credentials  (optional)
//	HA_TOPIC_PREFIX      value topic prefix  (default "jarvis")
//	HA_ENGINE_URL        model engine URL    (default http://localhost:11434)
//	HA_PUBLISH_INTERVAL  publish cadence     (default 30s)
//	HA_GPU_CMD           nvidia-smi path     (default nvidia-smi)
//
// JARVIS_DATA_DIR / RABBITMQ_URL / HA_JOB_LOG override the data sources.
package main

import (
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ImMakingAJarvis/ai-agent-build/internal/agentjob"
	"github.com/ImMakingAJarvis/ai-agent-build/internal/ha"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetPrefix("jarvis-ha: ")

	pub, err := ha.NewPublisher()
	if err != nil {
		log.Fatalf("%v", err)
	}
	if err := pub.Discover(); err != nil {
		log.Printf("discovery failed: %v", err)
	}

	engineURL := envOr("HA_ENGINE_URL", "http://localhost:11434")
	jobsLog := envOr("HA_JOB_LOG", filepath.Join(agentjob.DataDir(), "jobs.jsonl"))
	interval, err := time.ParseDuration(envOr("HA_PUBLISH_INTERVAL", "30s"))
	if err != nil || interval < time.Second {
		interval = 30 * time.Second
	}
	log.Printf("publishing to broker every %s (engine %s)", interval, engineURL)

	// Graceful shutdown: a clean MQTT DISCONNECT suppresses the last
	// will, so entities stay available across restarts instead of
	// flashing "unavailable" on a retained offline.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		log.Println("shutting down")
		pub.SetOffline()
		pub.Close()
		os.Exit(0)
	}()

	var last ha.State // last known-good values survive collector errors
	ticker := time.NewTicker(interval)
	for {
		s := last
		if d, err := ha.QueueDepths(); err != nil {
			log.Printf("queue depths: %v", err)
		} else {
			s.Queues = d
		}
		if t, err := ha.CurrentJob(); err != nil {
			log.Printf("current job: %v", err)
		} else {
			s.CurrentJob = t
		}
		if m, err := ha.LoadedModel(engineURL); err != nil {
			log.Printf("loaded model: %v", err)
		} else {
			s.LoadedModel = m
		}
		if c, err := ha.TodayAPISpend(jobsLog); err != nil {
			log.Printf("api spend: %v", err)
		} else {
			s.APISpendToday = c
		}
		if temp, power, err := ha.GPUStats(); err != nil {
			log.Printf("gpu: %v", err)
			s.GPUOK = false
		} else {
			s.GPUTemp, s.GPUPower, s.GPUOK = temp, power, true
		}
		if err := pub.PublishAll(s); err != nil {
			log.Printf("publish: %v", err)
			// MQTT is down; the LWT marks entities unavailable until
			// the broker connection recovers.
		} else {
			pub.SetOnline()
		}
		<-ticker.C
	}
}
