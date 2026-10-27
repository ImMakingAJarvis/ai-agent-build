#!/usr/bin/env python3
"""Generate the Jarvis Chat PWA icons: dark tile with an arc-reactor ring.

Run:  python3 scripts/gen_app_icons.py
Writes jarvis_app/icons/{icon-192.png,icon-512.png,apple-touch-icon.png}.
Re-run after changing the drawing; commit the resulting PNGs.
"""
import math
import os

from PIL import Image, ImageDraw

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "jarvis_app", "icons")
os.makedirs(OUT, exist_ok=True)

BG = (11, 15, 20, 255)        # #0b0f14 — matches the app theme
RING = (63, 182, 255, 255)    # #3fb6ff — accent
CORE = (150, 220, 255, 255)


def draw(size: int) -> Image.Image:
    img = Image.new("RGBA", (size, size), BG)
    d = ImageDraw.Draw(img)
    cx = cy = size / 2.0

    # Soft glow rings (outer → inner, increasing alpha).
    for r, a in ((0.40, 60), (0.36, 100), (0.32, 150)):
        d.ellipse([cx - size * r, cy - size * r, cx + size * r, cy + size * r],
                  outline=(63, 182, 255, a), width=max(1, int(size * 0.015)))

    # Main ring.
    rr = 0.26
    d.ellipse([cx - size * rr, cy - size * rr, cx + size * rr, cy + size * rr],
              outline=RING, width=max(2, int(size * 0.05)))

    # Arc-reactor spokes.
    for k in range(8):
        ang = math.tau * k / 8
        x0 = cx + math.cos(ang) * size * 0.135
        y0 = cy + math.sin(ang) * size * 0.135
        x1 = cx + math.cos(ang) * size * 0.245
        y1 = cy + math.sin(ang) * size * 0.245
        d.line([x0, y0, x1, y1], fill=RING, width=max(2, int(size * 0.03)))

    # Glowing core.
    cr = 0.10
    d.ellipse([cx - size * cr, cy - size * cr, cx + size * cr, cy + size * cr],
              fill=CORE, outline=RING, width=max(1, int(size * 0.02)))
    return img


def main():
    for name, size in (("icon-192.png", 192), ("icon-512.png", 512)):
        draw(size).save(os.path.join(OUT, name))
    # iOS wants a square, opaque apple-touch-icon.
    draw(180).convert("RGB").save(os.path.join(OUT, "apple-touch-icon.png"))
    print("wrote icons to", OUT)


if __name__ == "__main__":
    main()
