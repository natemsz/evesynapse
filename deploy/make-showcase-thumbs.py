#!/usr/bin/env python3
"""Regenerate the 800px thumbnails for the landing gallery.

The gallery reel shows each slide at about 660px wide on desktop and
287px on a phone, but the showcase files are 1036px wide (four are
1554px). PageSpeed flags every one of them as oversized. The 800px
thumb covers the reel at desktop density and on phones up to about
2.6x pixel density; srcset picks the full-size file on denser screens
and in the modal. The reel loads the *-thumb files; the modal loads
the full-size file (see landing.html and landing.js). Run this after
adding or re-encoding a showcase image.

Usage: python3 deploy/make-showcase-thumbs.py
Requires Pillow (pip install pillow). Writes <name>-thumb.<ext> beside
each full-size .jpg/.webp in internal/app/static/showcase/.
"""
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SHOWCASE = ROOT / "internal" / "app" / "static" / "showcase"
THUMB_WIDTH = 800
JPEG_QUALITY = 80
WEBP_QUALITY = 80


def main() -> int:
    try:
        from PIL import Image
    except ImportError:
        print("Pillow is required: pip install pillow", file=sys.stderr)
        return 1
    total_orig = total_thumb = 0
    made = 0
    for src in sorted(SHOWCASE.glob("*.jpg")) + sorted(SHOWCASE.glob("*.webp")):
        if src.name.endswith("-thumb" + src.suffix):
            continue
        dst = src.with_name(src.stem + "-thumb" + src.suffix)
        with Image.open(src) as im:
            if im.width <= THUMB_WIDTH:
                print(f"skip {src.name}: already {im.width}px wide")
                continue
            h = round(im.height * THUMB_WIDTH / im.width)
            thumb = im.resize((THUMB_WIDTH, h), Image.LANCZOS)
            save_kw = {"quality": JPEG_QUALITY} if src.suffix == ".jpg" else {"quality": WEBP_QUALITY}
            thumb.save(dst, **save_kw)
        total_orig += src.stat().st_size
        total_thumb += dst.stat().st_size
        made += 1
        print(f"{src.name} -> {dst.name} ({src.stat().st_size} -> {dst.stat().st_size} bytes)")
    print(f"{made} thumbnails, {total_orig} -> {total_thumb} bytes "
          f"({100 * (1 - total_thumb / total_orig):.0f}% smaller)" if total_orig else "nothing to do")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
