"""Validate SQLite recovery and consume first-start authorization outside the PVC."""

import json
import os
from pathlib import Path
import sqlite3
import ssl
import sys
from urllib.request import Request, urlopen


def integrity(database):
    if not database.is_file() or database.stat().st_size == 0:
        raise RuntimeError("SQLite recovery produced no database")
    connection = sqlite3.connect(database.as_uri() + "?mode=ro", uri=True)
    try:
        if connection.execute("PRAGMA integrity_check").fetchall() != [("ok",)]:
            raise RuntimeError("SQLite recovery failed its integrity check")
    finally:
        connection.close()


def prepare(mode, filename, namespace, state_name):
    database = Path(filename)
    restored = Path(filename + ".restoring")
    if mode == "verify":
        integrity(restored)
        print("SQLite restore integrity check succeeded", flush=True)
        return

    token = Path("/var/run/borq-sqlite/token").read_text().strip()
    context = ssl.create_default_context(cafile="/var/run/borq-sqlite/ca.crt")
    address = "https://kubernetes.default.svc/api/v1/namespaces/" + namespace + "/configmaps/" + state_name

    def state_request(method, payload=None):
        headers = {"Authorization": "Bearer " + token}
        body = None
        if payload is not None:
            body = json.dumps(payload).encode()
            headers["Content-Type"] = "application/merge-patch+json"
        with urlopen(Request(address, data=body, headers=headers, method=method), context=context, timeout=30) as response:
            return json.load(response)

    state = state_request("GET")
    if not database.exists() and not restored.exists() and state.get("data", {}).get("state") != "new":
        raise RuntimeError("SQLite recovery required: the database is missing and no backup was restored; refusing to create an empty database")

    if restored.exists() and not database.exists():
        integrity(restored)
    if state.get("data", {}).get("state") != "initialized":
        # The resource version makes first-start authorization consumable by only one initializer.
        state_request("PATCH", {"metadata": {"resourceVersion": state["metadata"]["resourceVersion"]}, "data": {"state": "initialized"}})
    if not database.exists():
        if not restored.exists():
            connection = sqlite3.connect(str(restored))
            connection.execute("PRAGMA user_version = 0")
            connection.close()
            integrity(restored)
        os.replace(restored, database)
    for file in (database, Path(filename + "-wal"), Path(filename + "-shm")):
        if file.exists():
            os.chown(file, -1, 2000)
            os.chmod(file, 0o660)


if __name__ == "__main__":
    try:
        prepare(*sys.argv[1:])
    except RuntimeError as error:
        print(str(error), file=sys.stderr)
        raise SystemExit(1)
    except Exception:
        # HTTP diagnostics may contain credentials; the pod logs need only the recovery action.
        print("SQLite recovery could not be verified. Check the backup and initialization state before explicitly resetting this database.", file=sys.stderr)
        raise SystemExit(1)
