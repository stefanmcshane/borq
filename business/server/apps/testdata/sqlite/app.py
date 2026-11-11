import http.server
import json
import os
import signal
import sqlite3
import sys

signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))


class App(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.respond(False)

    def do_POST(self):
        self.respond(True)

    def respond(self, write):
        with sqlite3.connect(os.environ["SQLITE_DATABASE_PATH"]) as db:
            db.execute("PRAGMA journal_mode=WAL")
            db.execute("PRAGMA synchronous=FULL")
            db.execute("CREATE TABLE IF NOT EXISTS records (id INTEGER PRIMARY KEY, value TEXT)")
            if write:
                db.execute("INSERT INTO records(value) VALUES ('preserved')")
            result = {
                "count": db.execute("SELECT count(*) FROM records").fetchone()[0],
                "integrity": db.execute("PRAGMA integrity_check").fetchone()[0],
                "uid": os.getuid(),
            }
        self.send_response(200)
        self.end_headers()
        self.wfile.write(json.dumps(result).encode())


http.server.HTTPServer(("0.0.0.0", 8080), App).serve_forever()
