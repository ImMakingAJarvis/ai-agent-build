"""Config flow for the Jarvis Voice integration."""

from __future__ import annotations

import aiohttp
import voluptuous as vol

from homeassistant.config_entries import ConfigFlow, ConfigFlowResult
from homeassistant.core import HomeAssistant

from .const import CONF_VOICE_URL, DEFAULT_VOICE_URL, DOMAIN


class JarvisVoiceConfigFlow(ConfigFlow, domain=DOMAIN):
    """Handle a config flow for Jarvis Voice."""

    VERSION = 1

    async def async_step_user(
        self, user_input: dict | None = None
    ) -> ConfigFlowResult:
        """Handle the user step."""
        errors: dict[str, str] = {}

        if user_input is not None:
            voice_url = user_input[CONF_VOICE_URL]

            # Validate connectivity
            try:
                async with aiohttp.ClientSession() as session:
                    async with session.get(
                        f"{voice_url}/health", timeout=aiohttp.ClientTimeout(total=10)
                    ) as resp:
                        if resp.status == 200:
                            body = await resp.json()
                            if body.get("status") != "ok":
                                errors["base"] = "degraded"
                        else:
                            errors["base"] = "cannot_connect"
            except Exception:
                errors["base"] = "cannot_connect"

            if not errors:
                return self.async_create_entry(
                    title=f"Jarvis Voice ({voice_url})",
                    data=user_input,
                )

        return self.async_show_form(
            step_id="user",
            data_schema=vol.Schema(
                {
                    vol.Required(
                        CONF_VOICE_URL, default=DEFAULT_VOICE_URL
                    ): str,
                }
            ),
            errors=errors,
        )
