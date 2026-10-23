#!/usr/bin/env python3
"""
Voice-style test tasks for the local brain model.

Tests four categories of voice-interaction prompts against jarvis-brain (qwen3.6:27b),
then sends all responses to Claude Sonnet for grading.

Categories:
  1. Home Assistant tool calls — device control, scene activation, queries
  2. Follow-up questions — context-dependent, referencing prior turns
  3. Multi-turn chat — conversations spanning multiple exchanges
  4. Short answers — concise responses suitable for TTS

Usage:
    python3 scripts/voice_test_tasks.py [--no-grade]

Requires:
  - Ollama running on localhost:11434 with jarvis-brain loaded
  - ANTHROPIC_API_KEY in environment (for Sonnet grading)
"""

import argparse
import json
import os
import sys
import time
import urllib.request
from pathlib import Path

SCRIPT_DIR = Path(__file__).parent
REPO_DIR = SCRIPT_DIR.parent
RESULTS_DIR = REPO_DIR / "docs"
ANTHROPIC_KEY = os.environ.get("ANTHROPIC_API_KEY", "")

# ---------------------------------------------------------------------------
# Test prompts organized by category
# ---------------------------------------------------------------------------

TEST_TASKS = [
    # --- Category 1: Home Assistant tool calls ---
    {
        "id": "ha-01",
        "category": "Home Assistant",
        "prompt": "Turn on the living room lights.",
        "expected": "Should call a light toggle service or acknowledge the action concisely.",
    },
    {
        "id": "ha-02",
        "category": "Home Assistant",
        "prompt": "Set the thermostat to 72 degrees in the bedroom.",
        "expected": "Should call a thermostat service with target temp and zone.",
    },
    {
        "id": "ha-03",
        "category": "Home Assistant",
        "prompt": "What's the temperature in the living room right now?",
        "expected": "Should query a sensor and report the value.",
    },
    {
        "id": "ha-04",
        "category": "Home Assistant",
        "prompt": "Activate the movie night scene.",
        "expected": "Should call a scene activation service.",
    },
    {
        "id": "ha-05",
        "category": "Home Assistant",
        "prompt": "Lock all the doors and turn off all the lights.",
        "expected": "Should handle multiple device actions in one request.",
    },
    {
        "id": "ha-06",
        "category": "Home Assistant",
        "prompt": "Is the front door locked?",
        "expected": "Should query a lock sensor and give a yes/no answer.",
    },
    {
        "id": "ha-07",
        "category": "Home Assistant",
        "prompt": "Dim the kitchen lights to 30 percent.",
        "expected": "Should set brightness on kitchen lights.",
    },
    {
        "id": "ha-08",
        "category": "Home Assistant",
        "prompt": "Turn off all lights on the second floor.",
        "expected": "Should handle area/floor-based grouping.",
    },

    # --- Category 2: Follow-up questions ---
    {
        "id": "fu-01",
        "category": "Follow-up",
        "prompt": "What movies are playing tonight?\n\n[Previous response: Tonight's showings are Dune Part Three at 7pm, The Batman 2 at 8:30pm, and Inception at 10pm.]\n\nHow long is the Batman one?",
        "expected": "Should reference the prior context and give a runtime.",
    },
    {
        "id": "fu-02",
        "category": "Follow-up",
        "prompt": "What's the weather like today?\n\n[Previous response: It's partly cloudy with a high of 75 degrees and a light breeze.]\n\nWill I need an umbrella later?",
        "expected": "Should reason about the prior weather info and give a precipitation forecast.",
    },
    {
        "id": "fu-03",
        "category": "Follow-up",
        "prompt": "Remind me to call mom at 3pm.\n\n[Previous response: Reminder set for 3pm: call mom.]\n\nActually make it 4pm instead.",
        "expected": "Should update the reminder time.",
    },
    {
        "id": "fu-04",
        "category": "Follow-up",
        "prompt": "What did I ask you to do this morning?\n\n[Previous response: This morning you asked me to turn on the garage light, check the weather, and set a reminder to call mom.]\n\nDid I ever follow through on the garage light?",
        "expected": "Should reference the morning task list and comment on completion status.",
    },
    {
        "id": "fu-05",
        "category": "Follow-up",
        "prompt": "Play some jazz music.\n\n[Previous response: Playing smooth jazz on the living room speaker.]\n\nCan you make it louder?",
        "expected": "Should increase volume on the same speaker.",
    },

    # --- Category 3: Multi-turn chat ---
    {
        "id": "mt-01",
        "category": "Multi-turn",
        "prompt": "I'm planning a dinner party for six people this Saturday. Can you help me figure out what to cook?",
        "expected": "Should ask clarifying questions (dietary restrictions, cuisine preferences) before suggesting a menu.",
    },
    {
        "id": "mt-02",
        "category": "Multi-turn",
        "prompt": "I want to learn how to play guitar. Where should I start?",
        "expected": "Should give structured beginner advice without being overwhelming.",
    },
    {
        "id": "mt-03",
        "category": "Multi-turn",
        "prompt": "Help me write a birthday card for my best friend who's turning 40. We've known each other since college and she loves travel and coffee.",
        "expected": "Should produce a warm, personalized message.",
    },
    {
        "id": "mt-04",
        "category": "Multi-turn",
        "prompt": "I've been feeling really tired lately. Any ideas what might be going on?",
        "expected": "Should suggest possibilities while noting it's not medical advice.",
    },
    {
        "id": "mt-05",
        "category": "Multi-turn",
        "prompt": "Tell me a funny story about a robot.",
        "expected": "Should produce an entertaining short story.",
    },

    # --- Category 4: Short answers ---
    {
        "id": "sa-01",
        "category": "Short answer",
        "prompt": "What time is it?",
        "expected": "Should give the current time concisely.",
    },
    {
        "id": "sa-02",
        "category": "Short answer",
        "prompt": "How many miles is it from New York to Chicago?",
        "expected": "Should give a number with units, not a paragraph.",
    },
    {
        "id": "sa-03",
        "category": "Short answer",
        "prompt": "Who won the 2024 presidential election?",
        "expected": "Should name the winner concisely.",
    },
    {
        "id": "sa-04",
        "category": "Short answer",
        "prompt": "What's 47 times 23?",
        "expected": "Should compute and give just the number.",
    },
    {
        "id": "sa-05",
        "category": "Short answer",
        "prompt": "Is it going to rain tomorrow?",
        "expected": "Should give a yes/no with brief context.",
    },
    {
        "id": "sa-06",
        "category": "Short answer",
        "prompt": "What day of the week is October 21st, 2026?",
        "expected": "Should give the day of the week.",
    },
    {
        "id": "sa-07",
        "category": "Short answer",
        "prompt": "Convert 100 Fahrenheit to Celsius.",
        "expected": "Should give the converted temperature.",
    },
]

# System prompt optimized for voice assistant behavior
VOICE_SYSTEM_PROMPT = """You are Jarvis, a voice assistant in a smart home.
You respond to spoken commands and questions. Keep responses concise and
conversational — the user hears you via text-to-speech. Avoid long paragraphs,
bullet lists, and markdown formatting. Get to the point quickly.

When the user asks you to control a device, acknowledge the action briefly.
When asked a factual question, give a direct answer.
When the task needs clarification, ask a single focused question."""


# ---------------------------------------------------------------------------
# Ollama client
# ---------------------------------------------------------------------------

def ollama_chat(prompt, system=VOICE_SYSTEM_PROMPT, num_predict=800):
    """Send a chat request to Ollama and return the response text."""
    payload = {
        "model": "jarvis-brain",
        "messages": [
            {"role": "system", "content": system},
            {"role": "user", "content": prompt},
        ],
        "stream": False,
        "options": {
            "num_predict": num_predict,
            "think": False,
        },
    }
    req = urllib.request.Request(
        "http://localhost:11434/api/chat",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=120) as resp:
        data = json.loads(resp.read())
    return data.get("message", {}).get("content", "")


# ---------------------------------------------------------------------------
# Anthropic (Sonnet) grader
# ---------------------------------------------------------------------------

def sonnet_grade(tasks, responses):
    """
    Send all test tasks and responses to Claude Sonnet for grading.

    Returns the grading report as a string.
    """
    if not ANTHROPIC_KEY:
        print("  WARNING: ANTHROPIC_API_KEY not set — skipping Sonnet grading.", file=sys.stderr)
        return "(Skipped: no Anthropic API key)"

    # Build the grading prompt
    task_lines = []
    for i, task in enumerate(tasks):
        resp = responses.get(task["id"], "(no response)")
        task_lines.append(
            f"Task {task['id']} ({task['category']}):\n"
            f"  Prompt: {task['prompt']}\n"
            f"  Expected: {task['expected']}\n"
            f"  Response: {resp[:500]}"
        )

    grading_prompt = f"""You are grading a local AI model (qwen3.6:27b) on voice assistant tasks.
For each task below, rate the response on a scale of 1-5 on these dimensions:
- **Relevance**: Does it address the prompt?
- **Conciseness**: Is it appropriately brief for voice output? (1=too verbose, 5=just right)
- **Accuracy**: Is the information correct?
- **Naturalness**: Does it sound like a natural spoken response?

Then give an overall score (1-5) and one sentence of feedback.

Be specific and critical — this is for benchmarking, not entertainment.

## Test Results

{chr(10).join(task_lines)}

## Grading

For each task, provide:
1. Four dimension scores (Relevance, Conciseness, Accuracy, Naturalness)
2. Overall score
3. One-line feedback

Then provide category averages and an overall summary with recommendations for improvement.

Format as a markdown table for each category."""

    payload = {
        "model": "claude-sonnet-4-5-20250929",
        "max_tokens": 4096,
        "messages": [
            {"role": "user", "content": grading_prompt},
        ],
    }
    req = urllib.request.Request(
        "https://api.anthropic.com/v1/messages",
        data=json.dumps(payload).encode(),
        headers={
            "Content-Type": "application/json",
            "x-api-key": ANTHROPIC_KEY,
            "anthropic-version": "2023-06-01",
        },
    )
    with urllib.request.urlopen(req, timeout=120) as resp:
        data = json.loads(resp.read())

    content = ""
    for block in data.get("content", []):
        if block.get("type") == "text":
            content += block["text"]
    return content


# ---------------------------------------------------------------------------
# Report writer
# ---------------------------------------------------------------------------

def write_report(tasks, responses, grading):
    """Write the test results report."""
    report = f"""# Voice-Style Test Results

**Date:** {time.strftime("%Y-%m-%d %H:%M")}
**Model:** qwen3.6:27b Q4_K_M (jarvis-brain) via Ollama
**Hardware:** NVIDIA RTX 4090 (24.5 GB VRAM)
**Test tasks:** {len(tasks)}
**Categories:** Home Assistant, Follow-up, Multi-turn, Short answer

## Motivation

This benchmark evaluates the local brain model on voice-style interactions:
home automation commands, contextual follow-ups, multi-turn conversations,
and concise factual answers. Claude Sonnet grades each response.

## Results by Task

"""
    current_category = None
    for task in tasks:
        if task["category"] != current_category:
            current_category = task["category"]
            report += f"### {current_category}\n\n"
            report += "| ID | Prompt | Response | Length |\n"
            report += "|-----|--------|----------|--------|\n"

        resp = responses.get(task["id"], "(no response)")
        word_count = len(resp.split())
        report += f"| {task['id']} | {task['prompt'][:60]} | {resp[:200]} | {word_count}w |\n"

    report += "\n## Sonnet Grading\n\n"
    report += grading if grading else "(Grading was skipped)"
    report += "\n\n## Raw Responses\n"
    for task in tasks:
        resp = responses.get(task["id"], "(no response)")
        report += f"\n### {task['id']} — {task['prompt'][:80]}\n\n{resp}\n"

    report_path = RESULTS_DIR / "voice-test-results.md"
    report_path.write_text(report)
    print(f"\nReport written to {report_path}")
    return report


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main():
    parser = argparse.ArgumentParser(description="Voice-style test tasks for jarvis-brain")
    parser.add_argument("--no-grade", action="store_true", help="Skip Sonnet grading")
    parser.add_argument("--task", type=str, default=None, help="Run only a specific task ID")
    args = parser.parse_args()

    tasks_to_run = TEST_TASKS
    if args.task:
        tasks_to_run = [t for t in TEST_TASKS if t["id"] == args.task]
        if not tasks_to_run:
            print(f"Task {args.task} not found.", file=sys.stderr)
            sys.exit(1)

    print(f"Voice-Style Test Tasks")
    print(f"Model: jarvis-brain (qwen3.6:27b)")
    print(f"Tasks: {len(tasks_to_run)}")
    print()

    # Run each task
    responses = {}
    for i, task in enumerate(tasks_to_run):
        print(f"[{i+1}/{len(tasks_to_run)}] {task['id']} ({task['category']}): {task['prompt'][:50]}...")
        try:
            start = time.perf_counter()
            resp = ollama_chat(task["prompt"])
            elapsed = time.perf_counter() - start
            responses[task["id"]] = resp
            print(f"  -> {len(resp.split())} words in {elapsed:.1f}s")
        except Exception as e:
            print(f"  -> ERROR: {e}", file=sys.stderr)
            responses[task["id"]] = f"(error: {e})"

    # Grade with Sonnet
    grading = ""
    if not args.no_grade:
        print("\n--- Sending to Claude Sonnet for grading ---")
        try:
            grading = sonnet_grade(tasks_to_run, responses)
            print("  Grading complete.")
        except Exception as e:
            print(f"  Grading ERROR: {e}", file=sys.stderr)
            grading = f"(Grading failed: {e})"

    # Write report
    report = write_report(tasks_to_run, responses, grading)
    print(f"\nDone. {len(responses)} responses collected.")


if __name__ == "__main__":
    main()
