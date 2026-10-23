"""Conversation agent for Jarvis Voice.

Delegates to the Jarvis voice service (STT → LLM → TTS) running on port 8100.
"""

from __future__ import annotations

import logging
from typing import Literal

import aiohttp

from homeassistant.components.conversation import (
    AbstractConversationAgent,
    ConversationInput,
    ConversationResult,
)
from homeassistant.components.conversation.error import ConversationError
from homeassistant.helpers import intent

__all__ = ["JarvisVoiceAgent"]

logger = logging.getLogger(__name__)


class JarvisVoiceAgent(AbstractConversationAgent):
    """Conversation agent that delegates to the Jarvis voice service."""

    supports_streaming = False

    def __init__(self, voice_url: str) -> None:
        self._voice_url = voice_url
        self._session: aiohttp.ClientSession | None = None

    @property
    def supported_languages(self) -> list[str] | Literal["*"]:
        return ["en"]

    async def async_process(
        self, user_input: ConversationInput
    ) -> ConversationResult:
        """Process a voice command via the Jarvis voice service."""
        if self._session is None:
            self._session = aiohttp.ClientSession()

        timeout = aiohttp.ClientTimeout(total=120)
        language = user_input.language

        try:
            async with self._session.post(
                f"{self._voice_url}/voice/json",
                json={"text": user_input.text},
                timeout=timeout,
            ) as resp:
                if resp.status != 200:
                    body = await resp.text()
                    raise ConversationError(
                        f"Jarvis returned status {resp.status}: {body}"
                    )

                # Capture response metadata from headers
                reply_text = resp.headers.get("X-Reply", user_input.text)
                elapsed = resp.headers.get("X-Elapsed", "?")
                logger.info(
                    "Jarvis replied in %ss: %s", elapsed, reply_text[:80]
                )

        except TimeoutError:
            raise ConversationError("Jarvis did not respond in time")
        except ConversationError:
            raise
        except Exception as err:
            logger.exception("Jarvis voice service error")
            raise ConversationError(f"Jarvis error: {err}") from err

        intent_response = intent.IntentResponse(language=language)
        intent_response.async_set_speech(reply_text)

        return ConversationResult(
            response=intent_response,
            conversation_id=user_input.conversation_id,
        )

    async def async_reload(self, language: str | None = None) -> None:
        """Reset conversation history on the voice service."""
        if self._session is None:
            self._session = aiohttp.ClientSession()
        try:
            async with self._session.post(
                f"{self._voice_url}/history/reset",
                timeout=aiohttp.ClientTimeout(total=10),
            ) as resp:
                logger.info("History reset: status %d", resp.status)
        except Exception:
            logger.exception("Failed to reset history")

    async def async_delete_agent(self) -> None:
        """Clean up the HTTP session."""
        if self._session:
            await self._session.close()
            self._session = None
