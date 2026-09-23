#!/usr/bin/env python3
"""Render the shared SVG mark into macOS and Windows app/tray resources.

Requires rsvg-convert and Pillow. Run from any working directory.
"""

from copy import deepcopy
from io import BytesIO
from pathlib import Path
import subprocess
import xml.etree.ElementTree as ET

from PIL import Image


ROOT = Path(__file__).resolve().parents[1]
DESKTOP = ROOT / "desktop"
SOURCE = DESKTOP / "assets/brand/localsubproxy.svg"
MAC_APP = DESKTOP / "macos/Runner/Assets.xcassets/AppIcon.appiconset"
MAC_TRAY = DESKTOP / "macos/Runner/Assets.xcassets/TrayIcon.imageset"
WINDOWS_APP = DESKTOP / "windows/runner/resources/app_icon.ico"
SVG_NS = "http://www.w3.org/2000/svg"


def render(svg: bytes, size: int) -> bytes:
    return subprocess.run(
        ["rsvg-convert", "--width", str(size), "--height", str(size)],
        input=svg,
        capture_output=True,
        check=True,
    ).stdout


def template_svg(source: bytes) -> bytes:
    ET.register_namespace("", SVG_NS)
    root = ET.fromstring(source)
    mark = root.find(f"{{{SVG_NS}}}g[@id='mark']")
    if mark is None:
        raise ValueError("logo SVG has no mark group")
    output = ET.Element(f"{{{SVG_NS}}}svg", {"viewBox": "0 0 128 128"})
    copied = deepcopy(mark)
    for element in copied.iter():
        for key in ("fill", "stroke"):
            if element.get(key) == "#FFFFFF":
                element.set(key, "#000000")
    output.append(copied)
    return ET.tostring(output, encoding="utf-8", xml_declaration=True)


def save_ico(source: bytes, destination: Path, sizes: list[int]) -> None:
    image = Image.open(BytesIO(render(source, 512))).convert("RGBA")
    destination.parent.mkdir(parents=True, exist_ok=True)
    image.save(destination, format="ICO", sizes=[(size, size) for size in sizes])


def main() -> None:
    source = SOURCE.read_bytes()
    for size in (16, 32, 64, 128, 256, 512, 1024):
        (MAC_APP / f"app_icon_{size}.png").write_bytes(render(source, size))

    mark = template_svg(source)
    MAC_TRAY.mkdir(parents=True, exist_ok=True)
    (MAC_TRAY / "tray_mark.png").write_bytes(render(mark, 18))
    (MAC_TRAY / "tray_mark@2x.png").write_bytes(render(mark, 36))
    (DESKTOP / "assets/brand/tray_mark.svg").write_bytes(mark)
    (DESKTOP / "assets/brand/localsubproxy_mark.png").write_bytes(render(source, 128))

    save_ico(source, WINDOWS_APP, [16, 24, 32, 48, 64, 128, 256])


if __name__ == "__main__":
    main()
