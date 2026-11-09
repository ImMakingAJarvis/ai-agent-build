#!/usr/bin/env python3
"""Display a small blue square on the screen.

Usage:
    python3 scripts/blue_square.py [size]

Draws a solid blue square, `size` pixels on a side (default 120), in its own
window. Works on Wayland or X11 (system GTK4 via PyGObject; needs the
python3-gi, python3-gtk-4.0 and python3-cairo packages). Close the window
(or press Esc) to exit.
"""

import sys

import cairo  # noqa: F401  # registers cairo.* types with PyGObject

import gi

gi.require_version("Gdk", "4.0")
gi.require_version("Gtk", "4.0")
from gi.repository import Gdk, Gtk


class BlueSquareApp(Gtk.Application):
    def __init__(self, size):
        super().__init__()
        self.size = size

    def do_activate(self):
        size = self.size

        def paint(_area, cr, _w, _h):
            cr.set_source_rgb(0.0, 0.0, 1.0)
            cr.paint()

        def on_key(_controller, keyval, _keycode, _state):
            if keyval == Gdk.Key.ESCAPE:
                self.quit()
            return Gtk.EventPropagation.STOP

        window = Gtk.ApplicationWindow(application=self)
        window.set_title("Blue square")
        area = Gtk.DrawingArea()
        area.set_content_width(size)
        area.set_content_height(size)
        area.set_draw_func(paint)
        window.set_child(area)
        window.set_default_size(size, size)
        key = Gtk.EventControllerKey.new()
        key.connect("key-pressed", on_key)
        window.add_controller(key)
        window.present()


def main() -> None:
    size = int(sys.argv[1]) if len(sys.argv) > 1 else 120
    BlueSquareApp(size).run([])  # no argv: our args are not GApplication options


if __name__ == "__main__":
    main()
