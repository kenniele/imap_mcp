import unittest
import importlib.util
from pathlib import Path

spec = importlib.util.spec_from_file_location("configure_oauth", Path(__file__).with_name("configure-oauth.py"))
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)
configure, value = helper.configure, helper.value


class ConfigureOAuthTest(unittest.TestCase):
    def setUp(self):
        self.original = (
            "# Existing installation\nPOSTGRES_PASSWORD=existing-db\n"
            "MCP_SECRET_KEY=existing-encryption-key\n"
            "MCP_AUTH_TOKEN=" + "t" * 32 + "\n"
            "AUTH_MODE=bearer\nMCP_HOST_PORT=18080\nUNRELATED=keep\n"
        )
        self.url = "https://imap-mcp.chickenkiller.com/mcp"

    def test_preserves_existing_credentials_and_stable_oauth_secrets(self):
        updated = configure(self.original, self.url)
        for key in ("POSTGRES_PASSWORD", "MCP_SECRET_KEY", "MCP_AUTH_TOKEN", "MCP_HOST_PORT", "UNRELATED"):
            self.assertEqual(value(updated, key), value(self.original, key))
        self.assertEqual(configure(updated, self.url), updated)
        self.assertEqual(value(updated, "AUTH_MODE"), "oauth")
        self.assertNotEqual(value(updated, "MCP_OAUTH_CLIENT_SECRET"), value(updated, "MCP_OAUTH_LOGIN_TOKEN"))

    def test_rejects_invalid_existing_secrets_and_public_urls(self):
        for text in (
            self.original.replace("MCP_SECRET_KEY=existing-encryption-key", "MCP_SECRET_KEY="),
            self.original + "MCP_OAUTH_CLIENT_SECRET=short\n",
            self.original + "MCP_OAUTH_LOGIN_TOKEN=" + "t" * 32 + "\n",
        ):
            with self.assertRaises(ValueError):
                configure(text, self.url)
        for url in ("http://mail.example/mcp", "https://mail.example/", "https://user:pass@mail.example/mcp"):
            with self.assertRaises(ValueError):
                configure(self.original, url)


if __name__ == "__main__":
    unittest.main()
