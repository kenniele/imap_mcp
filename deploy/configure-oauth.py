#!/usr/bin/env python3
"""Configure the server's existing .env without rotating mailbox encryption keys."""
import argparse
import os
import re
import secrets
import shutil
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlsplit


def value(text, key):
    matches = re.findall(r"^[ \t]*(?:export[ \t]+)?" + re.escape(key) + r"[ \t]*=[ \t]*(.*?)[ \t\r]*$", text, re.M)
    return matches[-1].strip("\"'") if matches else ""


def configure(text, public_url):
    url = urlsplit(public_url)
    if url.scheme != "https" or not url.hostname or url.path != "/mcp" or url.query or url.fragment or url.username or url.password:
        raise ValueError("Public URL must be an HTTPS address ending in /mcp")
    for key in ("POSTGRES_PASSWORD", "MCP_SECRET_KEY", "MCP_AUTH_TOKEN"):
        if not value(text, key):
            raise ValueError(f"Existing .env must contain {key}; do not replace the encryption key")
    client_secret = value(text, "MCP_OAUTH_CLIENT_SECRET") or secrets.token_hex(32)
    login_token = value(text, "MCP_OAUTH_LOGIN_TOKEN") or secrets.token_hex(32)
    if any(len(s) < 32 or re.search(r"\s", s) for s in (client_secret, login_token)):
        raise ValueError("Existing OAuth secrets must contain at least 32 characters without whitespace")
    if client_secret == login_token or any(s == value(text, "MCP_AUTH_TOKEN") for s in (client_secret, login_token)):
        raise ValueError("OAuth client/login secrets and MCP_AUTH_TOKEN must differ")
    settings = {
        "AUTH_MODE": "oauth",
        "MCP_PUBLIC_URL": public_url,
        "MCP_OAUTH_CLIENT_ID": value(text, "MCP_OAUTH_CLIENT_ID") or "mail-mcp-chatgpt",
        "MCP_OAUTH_CLIENT_SECRET": client_secret,
        "MCP_OAUTH_LOGIN_TOKEN": login_token,
        "MCP_OAUTH_REDIRECT_URIS": value(text, "MCP_OAUTH_REDIRECT_URIS") or "https://chatgpt.com/connector_platform_oauth_redirect",
        "MCP_PROXY_NETWORK": value(text, "MCP_PROXY_NETWORK") or "deployments_default",
    }
    for key, setting in settings.items():
        pattern = r"^[ \t]*(?:export[ \t]+)?" + re.escape(key) + r"[ \t]*=.*$"
        if re.search(pattern, text, re.M):
            text = re.sub(pattern, lambda _: key + "=" + setting, text, flags=re.M)
        else:
            text = text.rstrip("\n") + "\n" + key + "=" + setting + "\n"
    return text


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--env", default=".env")
    parser.add_argument("--public-url", default="https://imap-mcp.chickenkiller.com/mcp")
    args = parser.parse_args()
    path = Path(args.env).resolve(strict=True)
    updated = configure(path.read_text(), args.public_url)
    backup = path.with_name(path.name + ".bak." + datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S-%f"))
    shutil.copyfile(path, backup)
    backup.chmod(0o600)
    fd, tmp = tempfile.mkstemp(prefix=".env-oauth-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as out:
            out.write(updated)
        os.replace(tmp, path)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)
    print("OAuth configured. Existing database password, mailbox encryption key and metrics token preserved.")
    print("Secrets were saved only in .env; backup: " + str(backup))


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError) as err:
        raise SystemExit(str(err)) from None
