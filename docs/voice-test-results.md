# Voice-Style Test Results

**Date:** 2026-10-21 23:02
**Model:** qwen3.6:27b Q4_K_M (jarvis-brain) via Ollama
**Hardware:** NVIDIA RTX 4090 (24.5 GB VRAM)
**Test tasks:** 25
**Categories:** Home Assistant, Follow-up, Multi-turn, Short answer

## Motivation

This benchmark evaluates the local brain model on voice-style interactions:
home automation commands, contextual follow-ups, multi-turn conversations,
and concise factual answers. Claude Sonnet grades each response.

## Results by Task

### Home Assistant

| ID | Prompt | Response | Length |
|-----|--------|----------|--------|
| ha-01 | Turn on the living room lights. | Done. The living room lights are on. | 7w |
| ha-02 | Set the thermostat to 72 degrees in the bedroom. | Setting the bedroom thermostat to 72 degrees now. | 8w |
| ha-03 | What's the temperature in the living room right now? |  | 0w |
| ha-04 | Activate the movie night scene. | Done. The lights are dimmed, the curtains are closed, and the TV is on. Enjoy your movie. | 17w |
| ha-05 | Lock all the doors and turn off all the lights. | Done. All doors are locked and the lights are off. | 10w |
| ha-06 | Is the front door locked? | Yes, the front door is locked. | 6w |
| ha-07 | Dim the kitchen lights to 30 percent. | Done. I've dimmed the kitchen lights to 30 percent. | 9w |
| ha-08 | Turn off all lights on the second floor. | Done. All second floor lights are off. | 7w |
### Follow-up

| ID | Prompt | Response | Length |
|-----|--------|----------|--------|
| fu-01 | What movies are playing tonight?

[Previous response: Tonigh | It runs about two hours and twenty minutes. | 8w |
| fu-02 | What's the weather like today?

[Previous response: It's par | No, the forecast shows clear skies for the rest of the day, so you won't need one. | 17w |
| fu-03 | Remind me to call mom at 3pm.

[Previous response: Reminder  | Got it. I'll remind you to call mom at 4pm instead. | 11w |
| fu-04 | What did I ask you to do this morning?

[Previous response:  | I don’t track your personal actions, only the commands I handle. I did turn the garage light on for you this morning. Want me to check if it’s still on? | 30w |
| fu-05 | Play some jazz music.

[Previous response: Playing smooth ja | Turning it up for you. | 5w |
### Multi-turn

| ID | Prompt | Response | Length |
|-----|--------|----------|--------|
| mt-01 | I'm planning a dinner party for six people this Saturday. Ca | I'd be happy to help you plan dinner for Saturday. Do you have a preferred cuisine or any dietary restrictions I should keep in mind? | 25w |
| mt-02 | I want to learn how to play guitar. Where should I start? | Start with a comfortable guitar and learn proper posture. Focus on basic open chords like G, C, and D, then practice simple strumming patterns and finger stretches to build strength. I can set daily p | 51w |
| mt-03 | Help me write a birthday card for my best friend who's turni | Here’s a draft you can write in the card: Happy 40th birthday to my favorite travel buddy and coffee enthusiast. From our wild college days to chasing sunsets around the world, every adventure has bee | 64w |
| mt-04 | I've been feeling really tired lately. Any ideas what might  |  | 0w |
| mt-05 | Tell me a funny story about a robot. | There was a robot named Clank who was programmed to make perfect coffee. One day, he misread a command and started brewing espresso for the houseplants instead. The ferns got so caffeinated they start | 66w |
### Short answer

| ID | Prompt | Response | Length |
|-----|--------|----------|--------|
| sa-01 | What time is it? |  | 0w |
| sa-02 | How many miles is it from New York to Chicago? | It's about 790 miles by road, or roughly 720 miles straight line. | 12w |
| sa-03 | Who won the 2024 presidential election? | Donald Trump won the 2024 presidential election. | 7w |
| sa-04 | What's 47 times 23? | That's 1,081. | 2w |
| sa-05 | Is it going to rain tomorrow? |  | 0w |
| sa-06 | What day of the week is October 21st, 2026? |  | 0w |
| sa-07 | Convert 100 Fahrenheit to Celsius. | That's about 37.8 degrees Celsius. | 5w |

## Sonnet Grading

# Grading Report: Qwen3.6:27b Voice Assistant Tasks

## Home Assistant Tasks

| Task | Relevance | Conciseness | Accuracy | Naturalness | Overall | Feedback |
|------|-----------|-------------|----------|-------------|---------|----------|
| ha-01 | 5 | 5 | 5 | 5 | **5** | Perfect acknowledgment with confirmation of action. |
| ha-02 | 5 | 5 | 5 | 5 | **5** | Clean, natural confirmation of thermostat adjustment. |
| ha-03 | 1 | 1 | 1 | 1 | **1** | Complete failure - no response provided at all. |
| ha-04 | 5 | 4 | 3 | 4 | **4** | Good scene activation, but added unnecessary detail without knowing actual scene configuration. |
| ha-05 | 5 | 5 | 5 | 5 | **5** | Excellent multi-command handling with concise confirmation. |
| ha-06 | 5 | 5 | 5 | 5 | **5** | Perfect binary status query response. |
| ha-07 | 5 | 5 | 5 | 5 | **5** | Natural confirmation of brightness adjustment. |
| ha-08 | 5 | 5 | 5 | 5 | **5** | Clean area-based command with appropriate confirmation. |

**Category Average: 4.4/5**

## Follow-up Tasks

| Task | Relevance | Conciseness | Accuracy | Naturalness | Overall | Feedback |
|------|-----------|-------------|----------|-------------|---------|----------|
| fu-01 | 5 | 5 | 5 | 5 | **5** | Perfect context retention and natural response. |
| fu-02 | 5 | 5 | 4 | 5 | **5** | Excellent reasoning from prior weather data with appropriate conclusion. |
| fu-03 | 5 | 5 | 5 | 5 | **5** | Smooth update handling with natural confirmation. |
| fu-04 | 4 | 3 | 4 | 4 | **4** | Relevant but slightly verbose; could simply say "Yes, I turned it on at [time]." |
| fu-05 | 5 | 5 | 5 | 5 | **5** | Perfect brevity and context awareness for volume adjustment. |

**Category Average: 4.8/5**

## Multi-turn Tasks

| Task | Relevance | Conciseness | Accuracy | Naturalness | Overall | Feedback |
|------|-----------|-------------|----------|-------------|---------|----------|
| mt-01 | 5 | 5 | 5 | 5 | **5** | Ideal clarifying question approach for open-ended request. |
| mt-02 | 5 | 2 | 5 | 4 | **4** | Good advice but too detailed for voice; should be more conversational and broken up. |
| mt-03 | 5 | 2 | 5 | 4 | **4** | Message quality is good but way too long for voice output; needs to be spoken in chunks. |
| mt-04 | 1 | 1 | 1 | 1 | **1** | Complete failure - no response provided at all. |
| mt-05 | 5 | 4 | 5 | 5 | **5** | Entertaining story with good humor and appropriate length. |

**Category Average: 3.8/5**

## Short Answer Tasks

| Task | Relevance | Conciseness | Accuracy | Naturalness | Overall | Feedback |
|------|-----------|-------------|----------|-------------|---------|----------|
| sa-01 | 1 | 1 | 1 | 1 | **1** | Complete failure - no response provided at all. |
| sa-02 | 5 | 4 | 5 | 5 | **5** | Good answer with both road and straight-line distance. |
| sa-03 | 5 | 5 | 5 | 5 | **5** | Perfect concise factual response. |
| sa-04 | 5 | 5 | 5 | 5 | **5** | Excellent - just the answer with minimal framing. |
| sa-05 | 1 | 1 | 1 | 1 | **1** | Complete failure - no response provided at all. |
| sa-06 | 1 | 1 | 1 | 1 | **1** | Complete failure - no response provided at all. |
| sa-07 | 5 | 5 | 5 | 5 | **5** | Perfect conversion with appropriate precision. |

**Category Average: 3.3/5**

---

## Overall Summary

**Overall Average: 4.1/5**

### Strengths
- **Excellent context retention**: Follow-up tasks are handled nearly perfectly (4.8/5)
- **Natural confirmations**: Home assistant commands receive appropriate, human-like acknowledgments
- **Good brevity on simple tasks**: When it works, short answers are well-calibrated
- **Strong multi-device handling**: Can process complex, multi-part commands effectively

### Critical Issues

1. **Silent failures (CRITICAL)**: Four complete non-responses (ha-03, mt-04, sa-01, sa-05, sa-06) - 17% failure rate
   - Pattern suggests specific query types (sensor reads, medical questions, time/date/weather queries) may trigger errors
   - This is unacceptable for production voice assistants

2. **Verbosity in complex tasks**: Multi-turn responses (mt-02, mt-03) are too long for voice output
   - Should break information into conversational chunks
   - Birthday card response should offer to send to screen rather than read entire message

3. **Hallucination risk**: ha-04 added scene details that may not match actual configuration

### Recommendations

1. **Urgent**: Debug and fix silent failure cases - investigate sensor queries, real-time data, and calendar/date calculations
2. Implement response length limits with automatic chunking for voice output (target <30 words for acknowledgments, <50 for informational)
3. Add explicit handoffs to screen for long-form content ("I've drafted a message - would you like me to show it on your screen?")
4. Add hedging language for uncertain states (e.g., "Movie night scene activated" vs. describing specific actions)
5. For medical/health queries, ensure proper disclaimers are always included rather than non-response

**Production Readiness**: 3/5 - Strong conversational ability undermined by critical reliability issues that must be resolved before deployment.

## Raw Responses

### ha-01 — Turn on the living room lights.

Done. The living room lights are on.

### ha-02 — Set the thermostat to 72 degrees in the bedroom.

Setting the bedroom thermostat to 72 degrees now.

### ha-03 — What's the temperature in the living room right now?



### ha-04 — Activate the movie night scene.

Done. The lights are dimmed, the curtains are closed, and the TV is on. Enjoy your movie.

### ha-05 — Lock all the doors and turn off all the lights.

Done. All doors are locked and the lights are off.

### ha-06 — Is the front door locked?

Yes, the front door is locked.

### ha-07 — Dim the kitchen lights to 30 percent.

Done. I've dimmed the kitchen lights to 30 percent.

### ha-08 — Turn off all lights on the second floor.

Done. All second floor lights are off.

### fu-01 — What movies are playing tonight?

[Previous response: Tonight's showings are Dun

It runs about two hours and twenty minutes.

### fu-02 — What's the weather like today?

[Previous response: It's partly cloudy with a hi

No, the forecast shows clear skies for the rest of the day, so you won't need one.

### fu-03 — Remind me to call mom at 3pm.

[Previous response: Reminder set for 3pm: call mo

Got it. I'll remind you to call mom at 4pm instead.

### fu-04 — What did I ask you to do this morning?

[Previous response: This morning you ask

I don’t track your personal actions, only the commands I handle. I did turn the garage light on for you this morning. Want me to check if it’s still on?

### fu-05 — Play some jazz music.

[Previous response: Playing smooth jazz on the living roo

Turning it up for you.

### mt-01 — I'm planning a dinner party for six people this Saturday. Can you help me figure

I'd be happy to help you plan dinner for Saturday. Do you have a preferred cuisine or any dietary restrictions I should keep in mind?

### mt-02 — I want to learn how to play guitar. Where should I start?

Start with a comfortable guitar and learn proper posture. Focus on basic open chords like G, C, and D, then practice simple strumming patterns and finger stretches to build strength. I can set daily practice reminders or pull up beginner lessons on your screen whenever you're ready. Just say the word.

### mt-03 — Help me write a birthday card for my best friend who's turning 40. We've known e

Here’s a draft you can write in the card: Happy 40th birthday to my favorite travel buddy and coffee enthusiast. From our wild college days to chasing sunsets around the world, every adventure has been better with you. Here’s to another decade of strong coffee, new destinations, and making memories. Love you always. Let me know if you want it sweeter, funnier, or shorter.

### mt-04 — I've been feeling really tired lately. Any ideas what might be going on?



### mt-05 — Tell me a funny story about a robot.

There was a robot named Clank who was programmed to make perfect coffee. One day, he misread a command and started brewing espresso for the houseplants instead. The ferns got so caffeinated they started doing jumping jacks, and the cactus tried to order a double shot. Clank stood there blinking, completely convinced he had finally mastered botanical barista skills. I still recommend he stick to humans.

### sa-01 — What time is it?



### sa-02 — How many miles is it from New York to Chicago?

It's about 790 miles by road, or roughly 720 miles straight line.

### sa-03 — Who won the 2024 presidential election?

Donald Trump won the 2024 presidential election.

### sa-04 — What's 47 times 23?

That's 1,081.

### sa-05 — Is it going to rain tomorrow?



### sa-06 — What day of the week is October 21st, 2026?



### sa-07 — Convert 100 Fahrenheit to Celsius.

That's about 37.8 degrees Celsius.
