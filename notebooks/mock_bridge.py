"""Start only the bundled synthetic bridge and isolate its transaction storage."""

from __future__ import annotations

import hashlib
import importlib.util
import json
import os
from http.server import ThreadingHTTPServer
from pathlib import Path
from tempfile import TemporaryDirectory
from threading import Thread


class MockBridge:
    """Expose the repository fixture over loopback with an independent PUT audit."""

    def __init__(self):
        """Verify the fixture, discard inherited device settings and start it."""
        for key in list(os.environ):
            if key.startswith(("HUE_", "MOCK_")):
                os.environ.pop(key)
        self.temporary = TemporaryDirectory(prefix="vgi-hue-notebook-")
        os.environ["HUE_TX_DIR"] = self.temporary.name
        os.environ["HUE_DISCOVERY"] = "off"
        os.environ["HUE_DISCOVERY_MDNS"] = "off"
        folder = Path(__file__).parent / "fixtures"
        source = folder / "hue_bridge.py"
        expected = json.loads((folder / "provenance.json").read_text())["sha256"]
        if hashlib.sha256(source.read_bytes()).hexdigest() != expected:
            raise ValueError("Bundled synthetic bridge does not match its provenance hash")
        spec = importlib.util.spec_from_file_location("notebook_hue_fixture", source)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        module.APP_KEY = "notebook-test-key"
        self.put_paths = []
        paths = self.put_paths

        class Handler(module.Handler):
            def log_message(self, *_args):
                pass

            def do_PUT(self):
                paths.append(self.path)
                super().do_PUT()

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.url = f"http://127.0.0.1:{self.server.server_port}"

    def close(self):
        """Stop the local server and remove disposable transaction files."""
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)
        self.temporary.cleanup()
        os.environ.pop("HUE_TX_DIR", None)
