#!/usr/bin/env python3
"""Upsert a memory item with an optional image thumbnail via the X-Ray MCP server.

Usage:
  python upsert_memory.py --title "LaunchPad" --kind system_identity --content "..." [--image path/to/img.jpg] [--id existing-id]

Environment:
  XRAY_MCP_TOKEN  Bearer token for the MCP server
  XRAY_MCP_URL    MCP server URL (default: https://xray.example.com/mcp)
"""

import argparse, asyncio, base64, io, json, os, sys
from PIL import Image
from mcp import ClientSession
from mcp.client.streamable_http import streamablehttp_client

THUMB_MAX_PX = 480  # longest edge in pixels

URL = os.environ.get("XRAY_MCP_URL", "https://xray.example.com/mcp")
TOKEN = os.environ.get("XRAY_MCP_TOKEN", "")


def parse_args():
    p = argparse.ArgumentParser(description="Upsert an X-Ray memory item")
    p.add_argument("--id", help="Existing item ID to update")
    p.add_argument("--kind", choices=["stable_fact", "recent_change", "system_identity", "event", "agent_note"])
    p.add_argument("--scope", default="", choices=["", "global", "domain", "service"])
    p.add_argument("--title")
    p.add_argument("--content")
    p.add_argument("--subject", default="")
    p.add_argument("--confidence", default="", choices=["", "high", "medium", "low"])
    p.add_argument("--tags", nargs="*", default=[])
    p.add_argument("--event-date", default="", help="RFC3339 date for kind=event")
    p.add_argument("--image", help="Path to image file (JPEG/PNG) to embed as thumbnail")
    return p.parse_args()


async def fetch_existing(session, item_id: str) -> dict:
    """Fetch an existing memory item by ID over the authenticated MCP session.

    The REST route /v1/memory/{id} is OAuth-cookie-only and rejects the bearer
    token (401), so we read via the list_memory MCP tool on the same /mcp
    channel that authenticates the upsert, then match the id locally.
    """
    try:
        result = await session.call_tool("list_memory", {"limit": 1000})
        text = result.content[0].text if result.content else "{}"
        data = json.loads(text)
        items = data.get("items", data) if isinstance(data, dict) else data
        for it in (items or []):
            if it.get("id") == item_id:
                return it
        print(f"WARNING: item {item_id} not in list_memory results", file=sys.stderr)
        return {}
    except Exception as e:
        print(f"WARNING: could not fetch item {item_id}: {e}", file=sys.stderr)
        return {}


def strip_existing_thumbnail(content: str) -> str:
    """Remove an existing ![thumbnail](...) prefix from content."""
    import re
    return re.sub(r"^!\[thumbnail\]\(data:image/[^)]+\)\n*", "", content)


async def main():
    if not TOKEN:
        print("ERROR: XRAY_MCP_TOKEN not set", file=sys.stderr)
        sys.exit(1)

    args = parse_args()

    async with streamablehttp_client(URL, headers={"Authorization": f"Bearer {TOKEN}"}) as (r, w, _):
        async with ClientSession(r, w) as session:
            await session.initialize()

            existing: dict = {}
            if args.id:
                existing = await fetch_existing(session, args.id)
                if not existing:
                    print(f"WARNING: item {args.id} not found — will create with provided fields", file=sys.stderr)

            # Resolve fields: CLI args take precedence over existing values
            kind    = args.kind    or existing.get("kind", "")
            scope   = args.scope   or existing.get("scope", "global")
            title   = args.title   or existing.get("title", "")
            subject = args.subject or existing.get("subject", "")
            confidence = args.confidence or existing.get("confidence", "")
            tags    = args.tags if args.tags else existing.get("tags", [])

            # Content: use provided, or preserve existing (server handles thumbnail replacement)
            if args.content:
                content = args.content
            else:
                content = strip_existing_thumbnail(existing.get("content", ""))

            if not kind or not title:
                if args.id:
                    raise ValueError(f"could not fetch existing item {args.id} — supply --kind and --title explicitly")
                raise ValueError("--kind and --title are required for new items")

            params: dict = {
                "kind": kind,
                "scope": scope,
                "title": title,
                "content": content,
            }
            if args.id:
                params["id"] = args.id
            if subject:
                params["subject"] = subject
            if confidence:
                params["confidence"] = confidence
            if tags:
                params["tags"] = tags
            if args.event_date:
                params["event_date"] = args.event_date
            if args.image:
                img = Image.open(args.image).convert("RGB")
                img.thumbnail((THUMB_MAX_PX, THUMB_MAX_PX), Image.LANCZOS)
                buf = io.BytesIO()
                img.save(buf, format="JPEG", quality=25)
                params["image_data"] = base64.b64encode(buf.getvalue()).decode()
                params["image_mime"] = "image/jpeg"

            result = await session.call_tool("upsert_memory", params)
            text = result.content[0].text if result.content else "{}"
            print(json.dumps(json.loads(text), indent=2))


asyncio.run(main())
