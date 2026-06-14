"""Tools for the catalog-diff agent."""

from __future__ import annotations

import datetime
import pathlib

import requests
import yaml
from bs4 import BeautifulSoup

from runtime import tool

OPENCODE_DOC = "https://opencode.ai/docs/go"
OPENCODE_MODELS_API = "https://opencode.ai/zen/go/v1/models"
OPENCODE_GO_BASE_URL = "https://opencode.ai/zen/go/v1"
MODELS_DEV_API = "https://models.dev/api.json"
HTTP_HEADERS = {"User-Agent": "llmgate/catalog-drift", "Accept": "application/json,text/html"}
REPO = pathlib.Path(__file__).resolve().parents[2]
MODELS_DIR = REPO / "catalog" / "models"
ALIASES_DIR = REPO / "catalog" / "aliases"

_page_cache: dict[str, str] = {}
_json_cache: dict[str, dict] = {}

_CANONICAL_IDS = {
    # The Go docs table currently says "kimi-k2.7", while the docs prose
    # and the actual /models endpoint expose the usable id below.
    "kimi-k2.7": "kimi-k2.7-code",
}


def _fetch(url: str) -> str:
    if url not in _page_cache:
        r = requests.get(url, headers=HTTP_HEADERS, timeout=15)
        r.raise_for_status()
        _page_cache[url] = r.text
    return _page_cache[url]


def _fetch_json(url: str) -> dict:
    if url not in _json_cache:
        r = requests.get(url, headers=HTTP_HEADERS, timeout=15)
        r.raise_for_status()
        _json_cache[url] = r.json()
    return _json_cache[url]


def _canonical_model_id(model_id: str) -> str:
    return _CANONICAL_IDS.get(model_id, model_id)


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


def _auth_scheme_for_protocol(protocol: str) -> str:
    if protocol == "anthropic":
        return "x-api-key"
    return "bearer"


def _protocol_from_package(package_name: str | None) -> str | None:
    if package_name == "@ai-sdk/anthropic":
        return "anthropic"
    if package_name in {"@ai-sdk/openai-compatible", "@ai-sdk/alibaba"}:
        return "openai"
    return None


def _local_model_ids() -> set[str]:
    ids: set[str] = set()
    for p in MODELS_DIR.glob("*.yaml"):
        if p.name.endswith(".example"):
            continue
        data = yaml.safe_load(p.read_text()) or {}
        if data.get("vendor") != "opencode":
            continue
        base_url = str(data.get("base_url") or "").rstrip("/")
        if base_url != OPENCODE_GO_BASE_URL:
            continue
        model_id = data.get("id")
        if isinstance(model_id, str) and model_id:
            ids.add(model_id)
    return ids


def _load_aliases() -> dict[str, list[str]]:
    out: dict[str, list[str]] = {}
    for p in ALIASES_DIR.glob("*.yaml"):
        if p.name.endswith(".example"):
            continue
        data = yaml.safe_load(p.read_text()) or {}
        out[data.get("alias", p.stem)] = list(data.get("chain") or [])
    return out


def _remote_ids_from_models_api() -> set[str]:
    payload = _fetch_json(OPENCODE_MODELS_API)
    data = payload.get("data")
    if not isinstance(data, list):
        raise RuntimeError(f"{OPENCODE_MODELS_API}: expected data[]")
    out: set[str] = set()
    for item in data:
        if isinstance(item, dict) and isinstance(item.get("id"), str):
            out.add(_canonical_model_id(item["id"]))
    if not out:
        raise RuntimeError(f"{OPENCODE_MODELS_API}: no model ids returned")
    return out


def _models_dev() -> dict:
    return _fetch_json(MODELS_DEV_API)


def _model_spec(model_id: str) -> dict | None:
    """Return models.dev metadata for an OpenCode Go model.

    Prefer the opencode-go provider entry. When /v1/models has rolled out an
    id before models.dev adds it to opencode-go, fall back to the same model id
    under another provider so generated yaml still carries context/output
    specs instead of an empty stub.
    """
    providers = _models_dev()
    opencode_go = providers.get("opencode-go") or {}
    opencode_go_models = opencode_go.get("models") or {}
    if model_id in opencode_go_models:
        model = dict(opencode_go_models[model_id])
        provider_npm = model.get("provider", {}).get("npm") or opencode_go.get("npm")
        return {
            "model": model,
            "source_provider": "opencode-go",
            "package": provider_npm,
        }

    for provider_id, provider in sorted(providers.items()):
        models = provider.get("models") or {}
        if model_id not in models:
            continue
        model = dict(models[model_id])
        provider_npm = model.get("provider", {}).get("npm") or provider.get("npm")
        return {
            "model": model,
            "source_provider": provider_id,
            "package": provider_npm,
        }
    return None


def _protocols_from_docs_table(remote: list[dict]) -> dict[str, str]:
    out: dict[str, str] = {}
    for row in remote:
        model_id = row.get("id")
        protocol = row.get("protocol")
        if isinstance(model_id, str) and protocol in {"openai", "anthropic"}:
            out[_canonical_model_id(model_id)] = protocol
    return out


def _format_int(value: object) -> str:
    return f"{int(value):,}" if isinstance(value, (int, float)) else "unknown"


def _format_bool(value: object) -> str:
    return "yes" if value is True else "no" if value is False else "unknown"


def _format_modalities(values: object) -> str:
    if not isinstance(values, list) or not values:
        return "unknown"
    return ",".join(str(v) for v in values)


def _model_yaml(model_id: str, protocol: str, spec: dict) -> str:
    model = spec["model"]
    limit = model.get("limit") or {}
    modalities = model.get("modalities") or {}
    status = model.get("status")
    status_tail = f" ({status})" if status else ""
    lines = [
        f"# {model.get('name', model_id)} via OpenCode Go{status_tail}",
        f"# source: {MODELS_DEV_API} provider={spec['source_provider']}",
        (
            f"# context: {_format_int(limit.get('context'))} tokens / "
            f"max output: {_format_int(limit.get('output'))} tokens"
        ),
        (
            "# capabilities: "
            f"reasoning={_format_bool(model.get('reasoning'))}, "
            f"tool_call={_format_bool(model.get('tool_call'))}, "
            f"input={_format_modalities(modalities.get('input'))}, "
            f"output={_format_modalities(modalities.get('output'))}"
        ),
        f"id: {model_id}",
        "vendor: opencode",
        f"protocol: {protocol}",
        f"base_url: {OPENCODE_GO_BASE_URL}",
        "auth_env: LLMGATE_OPENCODE_API_KEY",
        f"auth_scheme: {_auth_scheme_for_protocol(protocol)}",
    ]
    return "\n".join(lines) + "\n"


@tool
def analyze_and_build_manifest(source_url: str, remote: list[dict]) -> dict:
    """Final step. Pass source_url and remote=[{"id":..., "protocol":"openai|anthropic"}, ...] from the docs endpoint table; /v1/models is fetched here as the authoritative id list. After calling this, stop."""
    remote_ids = _remote_ids_from_models_api()
    protocol_of = _protocols_from_docs_table(remote)
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
        alias_deletes: list[dict] = []
        for alias in referenced:
            before = list(aliases[alias])
            after = [m for m in before if m != stale_id]
            if not after:
                alias_deletes.append({
                    "kind": "delete_alias", "alias": alias,
                    "target": f"catalog/aliases/{alias}.yaml",
                    "reason": f"remove alias whose only model was deprecated {stale_id}",
                    "before": {"chain": before},
                    "after": {"deleted": True},
                })
            else:
                chain_updates.append({
                    "kind": "update_alias", "alias": alias,
                    "target": f"catalog/aliases/{alias}.yaml",
                    "reason": f"remove deprecated {stale_id} from chain",
                    "before": {"chain": before},
                    "after":  {"chain": after},
                })
        actions.extend(chain_updates)
        actions.extend(alias_deletes)
        actions.append({
            "kind": "delete_model", "model_id": stale_id,
            "target": f"catalog/models/{stale_id}.yaml",
            "reason": "not present on OpenCode Go models API",
            "safety": {"referenced_by_aliases": referenced},
        })

    for fresh_id in fresh:
        spec = _model_spec(fresh_id)
        if not spec:
            warnings.append({
                "kind": "missing_model_spec", "model_id": fresh_id,
                "message": f"'{fresh_id}' is listed by OpenCode Go but has no metadata in models.dev; skipping create_model.",
            })
            continue
        protocol = _protocol_from_package(spec.get("package")) or protocol_of.get(fresh_id)
        if not protocol:
            warnings.append({
                "kind": "ambiguous_protocol", "model_id": fresh_id,
                "message": f"protocol unknown for '{fresh_id}'; skipping create_model.",
            })
            continue
        actions.append({
            "kind": "create_model", "model_id": fresh_id,
            "target": f"catalog/models/{fresh_id}.yaml",
            "reason": "newly listed on OpenCode Go models API",
            "content": _model_yaml(fresh_id, protocol, spec),
            "protocol_source": (
                f"{MODELS_DEV_API} provider={spec['source_provider']} package={spec.get('package')}"
                if _protocol_from_package(spec.get("package"))
                else "AI SDK Package column on docs page"
            ),
            "spec_source": f"{MODELS_DEV_API} provider={spec['source_provider']}",
        })

    return {
        "final": True,
        "schema_version": 1,
        "generated_at": datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds"),
        "source": source_url,
        "model_source": OPENCODE_MODELS_API,
        "spec_source": MODELS_DEV_API,
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
