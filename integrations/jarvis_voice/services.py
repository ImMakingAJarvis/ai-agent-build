"""Services for the Jarvis Voice integration."""

from __future__ import annotations

import logging

import aiohttp

from homeassistant.core import HomeAssistant, ServiceCall

from .const import DOMAIN

logger = logging.getLogger(__name__)


async def async_setup_services(hass: HomeAssistant) -> None:
    """Register services for the Jarvis Voice integration."""

    async def handle_reset_history(call: ServiceCall) -> None:
        """Reset the conversation history on the voice service."""
        agent_data = hass.data.get(DOMAIN, {})
        if not agent_data:
            logger.warning("No Jarvis Voice agent loaded")
            return

        for entry_id, agent in agent_data.items():
            try:
                session = agent._session
                if session is None:
                    session = aiohttp.ClientSession()
                    agent._session = session
                await session.post(
                    f"{agent._voice_url}/history/reset",
                    auth=agent._auth,
                    timeout=aiohttp.ClientTimeout(total=10),
                )
                logger.info("History reset for agent %s", entry_id)
            except Exception:
                logger.exception("Failed to reset history for %s", entry_id)

    hass.services.async_register(
        DOMAIN,
        "reset_history",
        handle_reset_history,
    )
