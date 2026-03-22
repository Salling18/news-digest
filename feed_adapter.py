"""
feed_adapter.py — Typed wrapper around feedparser.

Feedparser's type stubs are incomplete, causing false positives throughout the
codebase. This module isolates all feedparser calls and returns plain typed
Python objects, so the rest of the pipeline never touches feedparser types
directly.
"""

from dataclasses import dataclass, field

import feedparser  # type: ignore[import]


@dataclass
class FeedEntry:
    link: str
    title: str
    body: str
    published_parsed: tuple | None
    updated_parsed: tuple | None


@dataclass
class FeedResult:
    status: int | None
    bozo: bool
    bozo_exception: Exception | None
    etag: str | None
    modified: str | None
    feed_link: str | None
    feed_title: str | None
    entries: list[FeedEntry] = field(default_factory=list)


def parse(url: str, *, agent: str, etag: str | None = None, modified: str | None = None) -> FeedResult:
    """Fetch and parse a feed URL, returning a typed FeedResult."""
    kwargs: dict = {"agent": agent}
    if etag:
        kwargs["etag"] = etag
    if modified:
        kwargs["modified"] = modified

    raw = feedparser.parse(url, **kwargs)
    feed_meta = raw.get("feed", {})

    entries = []
    for e in raw.get("entries", []):
        link = e.get("link") or e.get("id") or ""
        content = e.get("content")
        if content:
            body = content[0].get("value") or ""
        else:
            body = e.get("summary") or e.get("description") or ""
        entries.append(FeedEntry(
            link=str(link),
            title=str(e.get("title") or ""),
            body=str(body),
            published_parsed=e.get("published_parsed"),
            updated_parsed=e.get("updated_parsed"),
        ))

    return FeedResult(
        status=raw.get("status"),
        bozo=bool(raw.get("bozo", False)),
        bozo_exception=raw.get("bozo_exception"),
        etag=raw.get("etag"),
        modified=raw.get("modified"),
        feed_link=str(feed_meta.get("link") or "") or None,
        feed_title=str(feed_meta.get("title") or "") or None,
        entries=entries,
    )
