"""Tools for the catalog-diff agent."""

from __future__ import annotations

import datetime
import pathlib

import requests
import yaml
from bs4 import BeautifulSoup

from runtime import tool

OPENCODE_DOC = "https://opencode.ai/docs/go"
REPO = pathlib.Path(__file__).resolve().parents[2]
MODELS_DIR = REPO / "catalog" / "models"
ALIASES_DIR = REPO / "catalog" / "aliases"

_page_cache: dict[str, str] = {}


def _fetch(url: str) -> str:
    if url not in _page_cache:
        _page_cache[url] = requests.get(url, timeout=15).text
    return _page_cache[url]


@tool
def list_page_regions(url: str) -> dict:
    """Fetch a URL and return labeled candidate regions (tables, lists, headings). Call this first to see where the model catalog lives on the page."""
    html = _fetch(url)
    soup = BeautifulSoup(html, "html.parser")
    regions: list[dict] = []
    for i, t in enumerate(soup.find_all("table"), 1):
        regions.append({
            "label": f"table-{i}", "kind": "table",
            "headers": [th.get_text(strip=True) for th in t.find_all("th")],
            "row_count": max(len(t.find_all("tr")) - 1, 0),
        })
    for i, lst in enumerate(soup.find_all(["ul", "ol"]), 1):
        regions.append({
            "label": f"list-{i}", "kind": lst.name,
            "first_items": [li.get_text(strip=True) for li in lst.find_all("li")[:3]],
            "item_count": len(lst.find_all("li")),
        })
    for tag in ("h1", "h2", "h3"):
        for i, h in enumerate(soup.find_all(tag), 1):
            regions.append({"label": f"{tag}-{i}", "kind": tag, "text": h.get_text(strip=True)})
    return {"url": url, "regions": regions}


@tool
def extract_region(url: str, label: str) -> dict:
    """Return the HTML and plain text of one region by label (from list_page_regions). Use this once you have decided which region holds the catalog."""
    kind, _, idx_str = label.partition("-")
    try:
        idx = int(idx_str)
    except ValueError:
        return {"error": f"unparseable label: {label!r}"}
    soup = BeautifulSoup(_fetch(url), "html.parser")
    if kind == "table":
        elements = soup.find_all("table")
    elif kind == "list":
        elements = soup.find_all(["ul", "ol"])
    elif kind in ("h1", "h2", "h3"):
        elements = soup.find_all(kind)
    else:
        return {"error": f"unknown kind in label: {label!r}"}
    if not 1 <= idx <= len(elements):
        return {"error": f"label {label!r} out of range (have {len(elements)})"}
    el = elements[idx - 1]
    return {"label": label, "html": str(el), "text": el.get_text("\n", strip=True)}


_STUB_TEMPLATE = """\
# TODO: describe purpose & how this fits into alias chains
id: {model_id}
vendor: opencode
protocol: {protocol}
base_url: https://opencode.ai/zen/go/v1
auth_env: LLMGATE_OPENCODE_API_KEY
auth_scheme: bearer
"""


def _local_model_ids() -> set[str]:
    return {p.stem for p in MODELS_DIR.glob("*.yaml") if not p.name.endswith(".example")}


def _load_aliases() -> dict[str, list[str]]:
    out: dict[str, list[str]] = {}
    for p in ALIASES_DIR.glob("*.yaml"):
        if p.name.endswith(".example"):
            continue
        data = yaml.safe_load(p.read_text()) or {}
        out[data.get("alias", p.stem)] = list(data.get("chain") or [])
    return out


@tool
def analyze_and_build_manifest(source_url: str, remote: list[dict]) -> dict:
    """Final step. Pass source_url and remote=[{"id":..., "protocol":"openai|anthropic"}, ...]; receive the change manifest. Handles local catalog scan, alias chain impact, and stub yaml generation. After calling this, stop."""
    remote_ids = {m["id"] for m in remote}
    protocol_of = {m["id"]: m.get("protocol") for m in remote}
    local_ids = _local_model_ids()
    aliases = _load_aliases()

    refs: dict[str, list[str]] = {}
    for alias, chain in aliases.items():
        for mid in chain:
            refs.setdefault(mid, []).append(alias)

    stale = sorted(local_ids - remote_ids)
    fresh = sorted(remote_ids - local_ids)
    actions: list[dict] = []
    warnings: list[dict] = []

    for stale_id in stale:
        referenced = sorted(refs.get(stale_id, []))
        chain_updates: list[dict] = []
        orphan_blocks_delete = False
        for alias in referenced:
            before = list(aliases[alias])
            after = [m for m in before if m != stale_id]
            if not after:
                warnings.append({
                    "kind": "orphan_chain", "alias": alias,
                    "target": f"catalog/aliases/{alias}.yaml",
                    "message": f"Removing '{stale_id}' would leave '{alias}' chain empty. Manual decision required.",
                    "context": {"current_chain": before, "would_become": []},
                })
                orphan_blocks_delete = True
            else:
                chain_updates.append({
                    "kind": "update_alias", "alias": alias,
                    "target": f"catalog/aliases/{alias}.yaml",
                    "reason": f"remove deprecated {stale_id} from chain",
                    "before": {"chain": before},
                    "after":  {"chain": after},
                })
        if orphan_blocks_delete:
            continue
        actions.extend(chain_updates)
        actions.append({
            "kind": "delete_model", "model_id": stale_id,
            "target": f"catalog/models/{stale_id}.yaml",
            "reason": "not present on opencode docs page",
            "safety": {"referenced_by_aliases": referenced},
        })

    for fresh_id in fresh:
        protocol = protocol_of.get(fresh_id)
        if not protocol:
            warnings.append({
                "kind": "ambiguous_protocol", "model_id": fresh_id,
                "message": f"protocol unknown for '{fresh_id}'; skipping create_model.",
            })
            continue
        actions.append({
            "kind": "create_model", "model_id": fresh_id,
            "target": f"catalog/models/{fresh_id}.yaml",
            "reason": "newly listed on opencode docs page",
            "content": _STUB_TEMPLATE.format(model_id=fresh_id, protocol=protocol),
            "protocol_source": "AI SDK Package column on docs page",
        })

    return {
        "final": True,
        "schema_version": 1,
        "generated_at": datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds"),
        "source": source_url,
        "summary": {
            "local_count": len(local_ids),
            "remote_count": len(remote_ids),
            "stale_count": len(stale),
            "fresh_count": len(fresh),
            "action_count": len(actions),
            "warning_count": len(warnings),
        },
        "actions": actions,
        "warnings": warnings,
    }
