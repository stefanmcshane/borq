"""Exercise build publication and failed hot reload without a Kubernetes cluster."""

import os
import re
from pathlib import Path
import select
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.error
import urllib.request


ROOT = Path(__file__).resolve().parents[2]
LOCK = ROOT / "zarf/scripts/with-ui-lock.py"


class BuildTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="borq-build-test-")
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)

    def spawn(self, command, **kwargs):
        process = subprocess.Popen(command, start_new_session=True, **kwargs)

        def stop():
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.wait(timeout=5)
            for pipe in (process.stdin, process.stdout, process.stderr):
                if pipe:
                    pipe.close()

        self.addCleanup(stop)
        return process

    def wait_for(self, condition, message, timeout=15):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if condition():
                return
            time.sleep(0.05)
        self.fail(message)

    def test_lock_excludes_other_commands_and_releases_on_interruption(self):
        lock = self.root / "zarf/scripts/with-ui-lock.py"
        lock.parent.mkdir(parents=True)
        shutil.copy(LOCK, lock)
        owner = self.spawn(
            [sys.executable, str(lock), sys.executable, "-c",
             "print('locked', flush=True); input()"],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True,
        )
        self.assertTrue(select.select([owner.stdout], [], [], 5)[0], "lock owner did not start")
        self.assertEqual(owner.stdout.readline().strip(), "locked")
        waiter = self.spawn(
            [sys.executable, str(lock), sys.executable, "-c", "print('acquired', flush=True)"],
            stdout=subprocess.PIPE, text=True,
        )
        self.assertFalse(select.select([waiter.stdout], [], [], 0.3)[0], "commands overlapped")
        owner.terminate()
        owner.wait(timeout=5)
        self.assertEqual(waiter.communicate(timeout=5)[0].strip(), "acquired")
        self.assertEqual(waiter.returncode, 0)

    def test_failed_astro_build_preserves_the_complete_published_site(self):
        site = self.root / "api/webui/site"
        (site / "src/pages").mkdir(parents=True)
        (site / "scripts").mkdir()
        (self.root / "zarf/scripts").mkdir(parents=True)
        shutil.copy(LOCK, self.root / "zarf/scripts/with-ui-lock.py")
        shutil.copy(ROOT / "api/webui/site/scripts/build.mjs", site / "scripts/build.mjs")
        shutil.copy(ROOT / "api/webui/site/package.json", site / "package.json")
        (site / "node_modules").symlink_to(ROOT / "api/webui/site/node_modules", target_is_directory=True)
        page = site / "src/pages/index.astro"
        page.write_text("<h1>Last complete build</h1>\n")

        def build():
            return subprocess.run(
                ["npm", "run", "build", "--prefix", str(site)],
                cwd=self.root, capture_output=True, text=True, timeout=45,
                env={**os.environ, "NODE_OPTIONS": "--max-old-space-size=512"},
            )

        result = build()
        self.assertEqual(result.returncode, 0, (result.stdout + result.stderr)[-4000:])
        before = {str(p.relative_to(site / "dist")): p.read_bytes()
                  for p in (site / "dist").rglob("*") if p.is_file()}
        self.assertIn(b"Last complete build", before["index.html"])
        page.write_text("---\nconst = ;\n---\n<h1>Broken source</h1>\n")
        result = build()
        self.assertNotEqual(result.returncode, 0, "invalid Astro source unexpectedly built")
        self.assertIn("Expected", result.stdout + result.stderr)
        after = {str(p.relative_to(site / "dist")): p.read_bytes()
                 for p in (site / "dist").rglob("*") if p.is_file()}
        self.assertEqual(after, before, "failed build replaced the last complete site")

    def test_air_serves_last_successful_binary_after_a_build_failure(self):
        (self.root / "cmd").mkdir()
        source = self.root / "cmd/source.go"
        source.write_text("first")
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        (self.root / "app.py").write_text(
            "#!/usr/bin/env python3\n"
            "from http.server import BaseHTTPRequestHandler, HTTPServer\n"
            "class Handler(BaseHTTPRequestHandler):\n"
            " def do_GET(self):\n"
            "  self.send_response(200); self.end_headers(); self.wfile.write(b'__VERSION__')\n"
            " def log_message(self, *args): pass\n"
            f"HTTPServer(('127.0.0.1', {port}), Handler).serve_forever()\n"
        )
        (self.root / "build.py").write_text(
            "from pathlib import Path\n"
            "import sys\n"
            "version = Path('cmd/source.go').read_text()\n"
            "if version == 'fail': sys.exit(1)\n"
            "binary = Path('tmp/air/borq')\n"
            "binary.parent.mkdir(parents=True, exist_ok=True)\n"
            "binary.write_text(Path('app.py').read_text().replace('__VERSION__', version))\n"
            "binary.chmod(0o755)\n"
        )
        config = re.sub(
            r'^cmd = .+$', 'cmd = "python3 build.py"',
            (ROOT / "zarf/dev/.air.toml").read_text(), flags=re.MULTILINE,
        )
        (self.root / "air.toml").write_text(config)
        log = (self.root / "air.log").open("w")
        self.addCleanup(log.close)
        air = self.spawn(["air", "-c", "air.toml"], cwd=self.root, stdout=log, stderr=log)

        def serves(version):
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{port}/", timeout=0.5) as response:
                    return response.read(100).decode() == version
            except (OSError, urllib.error.URLError):
                return False

        self.wait_for(lambda: serves("first"), "initial Air binary did not start")
        source.write_text("fail")
        self.wait_for(lambda: "failed to build" in (self.root / "air.log").read_text(),
                      "negative case did not reach the failed build")
        time.sleep(1)
        self.wait_for(lambda: serves("first"), "failed build stopped the last successful version")
        self.assertIsNone(air.poll(), "Air stopped watching after a failed build")
        source.write_text("second")
        self.wait_for(lambda: serves("second"), "Air did not recover when source was fixed")


if __name__ == "__main__":
    unittest.main()
