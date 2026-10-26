"""Jarvis Voice integration for Home Assistant.

Connects Home Assistant's voice pipeline to the local Jarvis voice service
(STT → LLM → TTS) running on port 8100.
"""

from __future__ import annotations

import logging

from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant

from homeassistant.components.conversation.agent_manager import (
    get_agent_manager,
)
from .const import (
    CONF_PASSWORD,
    CONF_USERNAME,
    CONF_VOICE_URL,
    DEFAULT_VOICE_URL,
    DOMAIN,
)
from .conversation import JarvisVoiceAgent
from . import services

logger = logging.getLogger(__name__)


async def async_setup(hass: HomeAssistant, config: dict) -> bool:
    """Set up the Jarvis Voice integration."""
    await services.async_setup_services(hass)
    return True


async def async_setup_entry(hass: HomeAssistant, entry: ConfigEntry) -> bool:
    """Set up Jarvis Voice from a config entry."""
    voice_url = entry.data.get(CONF_VOICE_URL, DEFAULT_VOICE_URL)
    username = entry.data.get(CONF_USERNAME, "")
    password = entry.data.get(CONF_PASSWORD, "")
    agent = JarvisVoiceAgent(voice_url, username, password)

    # Register with the global agent manager
    manager = get_agent_manager(hass)
    manager.async_set_agent(entry.entry_id, agent)

    # Store for cleanup on unload
    hass.data.setdefault(DOMAIN, {})
    hass.data[DOMAIN][entry.entry_id] = agent

    entry.async_on_unload(entry.add_update_listener(_async_update_listener))
    logger.info("Jarvis Voice agent registered (url=%s)", voice_url)
    return True


async def _async_update_listener(
    hass: HomeAssistant, entry: ConfigEntry
) -> None:
    """Handle config entry options update."""
    await hass.config_entries.async_reload(entry.entry_id)


async def async_unload_entry(hass: HomeAssistant, entry: ConfigEntry) -> bool:
    """Unload a config entry."""
    agent = hass.data[DOMAIN].pop(entry.entry_id, None)
    if agent is not None:
        manager = get_agent_manager(hass)
        manager.async_unset_agent(entry.entry_id)
        await agent.async_delete_agent()
    return True
