"""Exercise deployment ordering/failure isolation without touching a Docker host."""
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path


class ReleaseTest(unittest.TestCase):
    def run_release(self, fail=""):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            release = root / "deploy/releases/test"
            release.mkdir(parents=True)
            (root / ".env").write_text("MCP_SECRET_KEY=existing-encryption-key\n")
            (release / "release.sh").write_text(Path(__file__).with_name("release.sh").read_text())
            (release / "docker-compose.prod.yml").write_text("services: {}\n")
            bin_dir = root / "bin"
            bin_dir.mkdir()
            log = root / "commands"
            docker = bin_dir / "docker"
            docker.write_text('''#!/usr/bin/env python3
import os,sys,json
args=sys.argv[1:]
with open(os.environ["COMMAND_LOG"],"a") as f: f.write(json.dumps(args)+"\\n")
if args[-1] == os.environ.get("FAIL_STAGE"): sys.exit(1)
if "json" in args:
 print(json.dumps({"services":{"mail-mcp":{"ports":[{"published":"18080"}],"environment":{"MCP_PUBLIC_URL":"https://imap-mcp.example/mcp"}}}}))
''')
            curl = bin_dir / "curl"
            curl.write_text('''#!/usr/bin/env python3
import sys,json
url=sys.argv[-1]
if "authorization-server" in url: print(json.dumps({"issuer":"https://imap-mcp.example","code_challenge_methods_supported":["S256"]}))
elif "protected-resource" in url: print(json.dumps({"resource":"https://imap-mcp.example/mcp","scopes_supported":["mail.read"]}))
elif "%{http_code}" in sys.argv: print("401",end="")
''')
            flock = bin_dir / "flock"
            flock.write_text("#!/usr/bin/env bash\nexit 0\n")
            for program in (docker, curl, flock):
                program.chmod(0o700)
            env = {**os.environ, "PATH": str(bin_dir) + os.pathsep + os.environ["PATH"], "COMMAND_LOG": str(log), "FAIL_STAGE": fail}
            image = "ghcr.io/kenniele/imap_mcp:" + "a" * 40
            result = subprocess.run(["bash", str(release / "release.sh"), str(root), image], env=env, capture_output=True, text=True)
            commands = [json.loads(line) for line in log.read_text().splitlines()]
            current = (root / "deploy/current").is_symlink()
            return result, commands, current

    def test_migrate_before_app_and_preserve_other_services(self):
        result, commands, current = self.run_release()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(current)
        check = next(i for i, c in enumerate(commands) if c[-1] == "check-config")
        migrate = next(i for i, c in enumerate(commands) if c[-1] == "migrate")
        start = next(i for i, c in enumerate(commands) if "up" in c and c[-1] == "mail-mcp")
        self.assertLess(check, migrate)
        self.assertLess(migrate, start)
        for command in commands:
            self.assertIn("imap_mcp", command)
            self.assertFalse(set(command) & {"down", "--remove-orphans", "caddy", "fitlog-caddy"})

    def test_failed_preflight_or_migration_does_not_replace_app(self):
        for stage in ("check-config", "migrate"):
            with self.subTest(stage=stage):
                result, commands, current = self.run_release(stage)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(current)
                self.assertFalse(any("up" in c and c[-1] == "mail-mcp" for c in commands))


if __name__ == "__main__":
    unittest.main()
