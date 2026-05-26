"""Import codex.json, kiro.json, antigravity.json from E:\\Tools\\cli\\AI
into per-account credential files under CLIProxyAPI/auths/, matching the
on-disk format each provider uses.

Run:
    python import_auths.py
"""

import json
import os
import re
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent
SRC_DIR = Path(r"E:/Tools/cli/AI")
AUTH_DIR = ROOT / "auths"


def safe_filename(part: str) -> str:
    cleaned = re.sub(r"[^A-Za-z0-9_.@+-]+", "_", part.strip())
    return cleaned or "anonymous"


def write_json(path: Path, data: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as f:
        json.dump(data, f, indent=2)


def import_codex(records: list[dict]) -> list[Path]:
    out = []
    for entry in records:
        if entry.get("provider") != "codex":
            continue
        email = entry.get("email") or entry.get("name") or "user"
        psd = entry.get("providerSpecificData") or {}
        plan = (psd.get("chatgptPlanType") or "").lower()
        suffix = f"-{plan}" if plan else ""
        fname = f"codex-{safe_filename(email)}{suffix}.json"
        record = {
            "type": "codex",
            "id_token": entry.get("idToken", ""),
            "access_token": entry.get("accessToken", ""),
            "refresh_token": entry.get("refreshToken", ""),
            "account_id": psd.get("chatgptAccountId", ""),
            "email": email,
            "expired": entry.get("expiresAt", ""),
            "last_refresh": entry.get("updatedAt", ""),
        }
        if plan:
            record["plan_type"] = plan
        write_json(AUTH_DIR / fname, record)
        out.append(AUTH_DIR / fname)
    return out


def import_antigravity(records: list[dict]) -> list[Path]:
    out = []
    for entry in records:
        if entry.get("provider") != "antigravity":
            continue
        email = entry.get("email") or entry.get("name") or "user"
        fname = f"antigravity-{safe_filename(email)}.json"
        record = {
            "type": "antigravity",
            "access_token": entry.get("accessToken", ""),
            "refresh_token": entry.get("refreshToken", ""),
            "expires_in": entry.get("expiresIn", 0),
            "expired": entry.get("expiresAt", ""),
            "email": email,
            "timestamp": int(time.time() * 1000),
        }
        if entry.get("projectId"):
            record["project_id"] = entry["projectId"]
        if entry.get("scope"):
            record["scope"] = entry["scope"]
        write_json(AUTH_DIR / fname, record)
        out.append(AUTH_DIR / fname)
    return out


def import_kiro(records: list[dict]) -> list[Path]:
    out = []
    for idx, entry in enumerate(records):
        if entry.get("provider") != "kiro":
            continue
        psd = entry.get("providerSpecificData") or {}
        auth_method = psd.get("authMethod") or ("builder-id" if psd.get("clientId") else "social")
        label = entry.get("email") or entry.get("name") or f"kiro-{idx+1}"
        fname = f"kiro-{safe_filename(label)}.json"
        record = {
            "type": "kiro",
            "auth_method": auth_method,
            "access_token": entry.get("accessToken", ""),
            "refresh_token": entry.get("refreshToken", ""),
            "expired": entry.get("expiresAt", ""),
        }
        if psd.get("clientId"):
            record["client_id"] = psd["clientId"]
        if psd.get("clientSecret"):
            record["client_secret"] = psd["clientSecret"]
        if psd.get("region"):
            record["region"] = psd["region"]
        if psd.get("profileArn"):
            record["profile_arn"] = psd["profileArn"]
        if entry.get("email"):
            record["email"] = entry["email"]
        elif entry.get("name"):
            record["email"] = entry["name"]
        write_json(AUTH_DIR / fname, record)
        out.append(AUTH_DIR / fname)
    return out


def remove_existing_provider_files(prefix: str) -> int:
    if not AUTH_DIR.exists():
        return 0
    removed = 0
    for path in AUTH_DIR.iterdir():
        if path.is_file() and path.name != ".gitkeep" and path.name.startswith(prefix):
            path.unlink()
            removed += 1
    return removed


def main() -> int:
    if not SRC_DIR.exists():
        print(f"Source dir missing: {SRC_DIR}", file=sys.stderr)
        return 1

    AUTH_DIR.mkdir(parents=True, exist_ok=True)

    summary = {"codex": 0, "antigravity": 0, "kiro": 0, "removed": {}}

    for prefix in ("codex-", "antigravity-", "kiro-"):
        summary["removed"][prefix] = remove_existing_provider_files(prefix)

    for src_name, importer, key in (
        ("codex.json", import_codex, "codex"),
        ("antigravity.json", import_antigravity, "antigravity"),
        ("kiro.json", import_kiro, "kiro"),
    ):
        src = SRC_DIR / src_name
        if not src.exists():
            print(f"warning: {src} not found, skipping", file=sys.stderr)
            continue
        with src.open("r", encoding="utf-8") as f:
            records = json.load(f)
        if not isinstance(records, list):
            print(f"warning: {src} is not a list, skipping", file=sys.stderr)
            continue
        written = importer(records)
        summary[key] = len(written)

    print("Import summary:")
    for k, v in summary.items():
        print(f"  {k}: {v}")
    print(f"Auth dir: {AUTH_DIR}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
