"""Local delivery state for one host (SQLite).

Delivery state never travels in an envelope. The poller and the in-agent tool
may run at the same time in different processes, so every hand-off is a single
transaction that either claims the row or loses the race.
"""
from __future__ import annotations

from contextlib import closing, contextmanager
from pathlib import Path
import os
import sqlite3
import time

SCHEMA = """
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS seen (honcho_id TEXT PRIMARY KEY, at REAL NOT NULL);
CREATE TABLE IF NOT EXISTS parts (
    msg_id TEXT NOT NULL, part INTEGER NOT NULL, total INTEGER NOT NULL,
    sender TEXT NOT NULL, provenance TEXT NOT NULL, wire TEXT NOT NULL,
    PRIMARY KEY (msg_id, part));
CREATE TABLE IF NOT EXISTS handled (
    msg_id TEXT PRIMARY KEY, sender TEXT NOT NULL, action TEXT NOT NULL, reason TEXT NOT NULL,
    at REAL NOT NULL);
CREATE TABLE IF NOT EXISTS turns (session_id TEXT PRIMARY KEY, woken INTEGER NOT NULL, at REAL NOT NULL);
CREATE TABLE IF NOT EXISTS inbox (
    msg_id TEXT PRIMARY KEY, sender TEXT NOT NULL, ref TEXT NOT NULL, kind TEXT NOT NULL,
    reason TEXT NOT NULL, wire TEXT NOT NULL, at REAL NOT NULL, delivered_at REAL);
CREATE TABLE IF NOT EXISTS wakes (
    msg_id TEXT PRIMARY KEY, status TEXT NOT NULL, started_at REAL NOT NULL,
    finished_at REAL, detail TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS hops (msg_id TEXT PRIMARY KEY, hop INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS outbox (
    msg_id TEXT NOT NULL, part INTEGER NOT NULL, total INTEGER NOT NULL, ref TEXT NOT NULL,
    recipients TEXT NOT NULL, status TEXT NOT NULL, honcho_id TEXT NOT NULL DEFAULT '',
    at REAL NOT NULL, PRIMARY KEY (msg_id, part));
"""
# hops: the chain depth of mail that started a turn here. A table of its own, so a state.db made
# by an older version gains it on open and loses nothing if the older version runs again.
# inbox.delivered_at: NULL = waiting for the next turn, HELD = reserved for a turn the poller is
# about to start, anything else = the time it was shown to the agent.
HELD = -1.0


def _now(now: float | None) -> float:
    return time.time() if now is None else now


class Store:
    def __init__(self, path: Path):
        self._path = Path(path)
        self._path.parent.mkdir(parents=True, exist_ok=True)
        with closing(self._connect()) as db:
            db.executescript(SCHEMA)  # executescript manages its own transaction
        if os.name == "posix":
            os.chmod(self._path, 0o600)

    def _connect(self) -> sqlite3.Connection:
        db = sqlite3.connect(self._path, timeout=15, isolation_level=None)
        db.execute("PRAGMA journal_mode=WAL")
        db.execute("PRAGMA busy_timeout=15000")
        return db

    @contextmanager
    def _tx(self):
        with closing(self._connect()) as db:
            db.execute("BEGIN IMMEDIATE")
            try:
                yield db
            except BaseException:
                db.execute("ROLLBACK")
                raise
            db.execute("COMMIT")

    def _meta(self, key: str) -> str | None:
        with self._tx() as db:
            row = db.execute("SELECT value FROM meta WHERE key=?", (key,)).fetchone()
        return row[0] if row else None

    def get_meta(self, key: str) -> str | None:
        return self._meta(key)

    def set_meta(self, key: str, value: str | None) -> None:
        with self._tx() as db:
            if value is None:
                db.execute("DELETE FROM meta WHERE key=?", (key,))
            else:
                db.execute("INSERT OR REPLACE INTO meta VALUES (?, ?)", (key, str(value)))

    # ----- baseline, freshness and dedupe ----------------------------------------

    def baseline_time(self) -> float | None:
        value = self._meta("baseline")
        return float(value) if value is not None else None

    def has_baseline(self) -> bool:
        return self.baseline_time() is not None

    def set_baseline(self, honcho_ids: list[str], now: float | None = None) -> None:
        """First contact: everything already in the mailbox is history, not new mail."""
        now = _now(now)
        with self._tx() as db:
            db.executemany("INSERT OR IGNORE INTO seen VALUES (?, ?)", [(i, now) for i in honcho_ids])
            db.execute("INSERT OR REPLACE INTO meta VALUES ('baseline', ?)", (repr(now),))

    def touch_sync(self, now: float | None = None) -> None:
        with self._tx() as db:
            db.execute("INSERT OR REPLACE INTO meta VALUES ('last_sync', ?)", (repr(_now(now)),))

    def last_sync(self) -> float:
        return float(self._meta("last_sync") or 0.0)

    def is_seen(self, honcho_id: str) -> bool:
        with self._tx() as db:
            return db.execute("SELECT 1 FROM seen WHERE honcho_id=?", (honcho_id,)).fetchone() is not None

    def mark_seen(self, honcho_id: str, now: float | None = None) -> None:
        with self._tx() as db:
            db.execute("INSERT OR IGNORE INTO seen VALUES (?, ?)", (honcho_id, _now(now)))

    # ----- multipart assembly ----------------------------------------------------

    def add_part(self, msg_id: str, part: int, total: int, sender: str, provenance: str, wire: str) -> list[str]:
        """Store one part; return every wire once the logical message is complete, else []."""
        with self._tx() as db:
            db.execute("INSERT OR IGNORE INTO parts VALUES (?, ?, ?, ?, ?, ?)",
                       (msg_id, part, total, sender, provenance, wire))
            rows = db.execute(
                "SELECT part, total, sender, provenance, wire FROM parts WHERE msg_id=? ORDER BY part",
                (msg_id,)).fetchall()
        consistent = len({(r[1], r[2], r[3]) for r in rows}) == 1
        if consistent and [r[0] for r in rows] == list(range(1, total + 1)):
            return [r[4] for r in rows]
        return []

    # ----- one decision per logical message --------------------------------------

    def settle(self, msg_id: str, sender: str, action: str, reason: str, *, ref: str = "", kind: str = "",
               wire: str = "", hop: int | None = None, now: float | None = None) -> bool:
        """Record the one decision for a message and, in the same transaction, its inbox row.
        A wake keeps a held inbox row so the mail survives a crash before or during the turn,
        and its hop count so that what the turn sends can be counted one deeper."""
        now = _now(now)
        with self._tx() as db:
            cur = db.execute("INSERT OR IGNORE INTO handled VALUES (?, ?, ?, ?, ?)",
                             (msg_id, sender, action, reason, now))
            if cur.rowcount != 1:
                return False
            if action in ("inbox", "wake"):
                db.execute("INSERT OR IGNORE INTO inbox VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                           (msg_id, sender, ref, kind, reason, wire, now, HELD if action == "wake" else None))
            if action == "wake" and hop is not None:
                db.execute("INSERT OR IGNORE INTO hops VALUES (?, ?)", (msg_id, int(hop)))
            return True

    def handled(self, msg_id: str) -> tuple[str, str] | None:
        with self._tx() as db:
            return db.execute("SELECT action, reason FROM handled WHERE msg_id=?", (msg_id,)).fetchone()

    def wakes_since(self, sender: str, since: float) -> int:
        """How many turns mail from ``sender`` has started here since ``since``."""
        with self._tx() as db:
            return db.execute("SELECT COUNT(*) FROM handled WHERE sender=? AND action='wake' AND at>=?",
                              (sender, since)).fetchone()[0]

    def sent_on(self, ref: str) -> bool:
        """Whether this host has written anything in the thread ``ref``."""
        with self._tx() as db:
            return db.execute("SELECT 1 FROM outbox WHERE ref=? LIMIT 1", (ref,)).fetchone() is not None

    # ----- which local turns geminise itself started -----------------------------

    def mark_turn(self, session_id: str, woken: bool, now: float | None = None) -> None:
        now = _now(now)
        with self._tx() as db:
            db.execute("INSERT OR REPLACE INTO turns VALUES (?, ?, ?)", (session_id, 1 if woken else 0, now))
            db.execute("DELETE FROM turns WHERE at < ?", (now - 7 * 86400,))

    def turn_woken(self, session_id: str) -> bool:
        with self._tx() as db:
            row = db.execute("SELECT woken FROM turns WHERE session_id=?", (session_id,)).fetchone()
        return bool(row and row[0])

    # ----- inbox: surfaced at the agent's next turn ------------------------------

    def take_inbox(self, *, ref: str | None = None, limit: int = 20, now: float | None = None) -> list[dict]:
        """Return waiting items oldest first and mark them shown in the same transaction."""
        query = "SELECT msg_id, sender, ref, kind, reason, wire, at FROM inbox WHERE delivered_at IS NULL"
        args: tuple = ()
        if ref:
            query, args = query + " AND ref=?", (ref,)
        with self._tx() as db:
            rows = db.execute(query + " ORDER BY at, msg_id LIMIT ?", (*args, limit)).fetchall()
            db.executemany("UPDATE inbox SET delivered_at=? WHERE msg_id=?", [(_now(now), r[0]) for r in rows])
        keys = ("id", "from", "ref", "kind", "reason", "wire", "at")
        return [dict(zip(keys, row)) for row in rows]

    def inbox_pending(self) -> int:
        with self._tx() as db:
            return db.execute("SELECT COUNT(*) FROM inbox WHERE delivered_at IS NULL").fetchone()[0]

    # ----- wakes: at most one local turn per message -----------------------------

    def claim_wake(self, msg_id: str, now: float | None = None) -> bool:
        with self._tx() as db:
            cur = db.execute("INSERT OR IGNORE INTO wakes (msg_id, status, started_at) VALUES (?, 'running', ?)",
                             (msg_id, _now(now)))
            return cur.rowcount == 1

    def finish_wake(self, msg_id: str, ok: bool, detail: str = "", now: float | None = None) -> None:
        """Close a wake. On success the held mail counts as shown; otherwise it waits in the inbox."""
        now = _now(now)
        with self._tx() as db:
            db.execute("UPDATE wakes SET status=?, finished_at=?, detail=? WHERE msg_id=?",
                       ("done" if ok else "failed", now, detail[:500], msg_id))
            if ok:
                db.execute("UPDATE inbox SET delivered_at=? WHERE msg_id=? AND delivered_at=?", (now, msg_id, HELD))
            else:
                db.execute("UPDATE inbox SET delivered_at=NULL, reason='wake_failed' WHERE msg_id=? AND delivered_at=?",
                           (msg_id, HELD))

    def wake(self, msg_id: str) -> dict | None:
        with self._tx() as db:
            row = db.execute("SELECT status, started_at, finished_at, detail FROM wakes WHERE msg_id=?",
                             (msg_id,)).fetchone()
        return dict(zip(("status", "started_at", "finished_at", "detail"), row)) if row else None

    def wake_running(self, max_age: float, now: float | None = None) -> bool:
        """Whether a turn started by geminise may be running on this host right now."""
        with self._tx() as db:
            return db.execute("SELECT 1 FROM wakes WHERE status='running' AND started_at>=? LIMIT 1",
                              (_now(now) - max_age,)).fetchone() is not None

    def wake_hop(self, msg_id: str, max_age: float, now: float | None = None) -> int | None:
        """The hop count of the mail ``msg_id``, while the turn it started may still be running.
        None once that turn is finished, abandoned or past ``max_age``, and when no count was recorded."""
        with self._tx() as db:
            row = db.execute(
                "SELECT h.hop FROM wakes w JOIN hops h ON h.msg_id=w.msg_id "
                "WHERE w.msg_id=? AND w.status='running' AND w.started_at>=?",
                (msg_id, _now(now) - max_age)).fetchone()
        return row[0] if row else None

    def recover(self, max_age: float, now: float | None = None) -> int:
        """Release mail whose turn never finished (crash, kill): it waits in the inbox instead."""
        cutoff = _now(now) - max_age
        with self._tx() as db:
            db.execute("UPDATE wakes SET status='abandoned', finished_at=? WHERE status='running' AND started_at<?",
                       (_now(now), cutoff))
            cur = db.execute(
                "UPDATE inbox SET delivered_at=NULL, reason='wake_failed' WHERE delivered_at=? AND at<? AND msg_id "
                "NOT IN (SELECT msg_id FROM wakes WHERE status='running')", (HELD, cutoff))
            return cur.rowcount

    # ----- outbox: what this host sent -------------------------------------------

    def record_out(self, msg_id: str, part: int, total: int, ref: str, recipients: str, status: str,
                   honcho_id: str = "", now: float | None = None) -> None:
        with self._tx() as db:
            db.execute("INSERT OR REPLACE INTO outbox VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                       (msg_id, part, total, ref, recipients, status, honcho_id, _now(now)))

    def summary(self) -> dict:
        last = self.last_sync()
        with self._tx() as db:
            count = lambda sql: db.execute(sql).fetchone()[0]  # noqa: E731
            return {
                "baseline": count("SELECT COUNT(*) FROM meta WHERE key='baseline'") == 1,
                "seconds_since_last_read": round(time.time() - last) if last else None,
                "seen": count("SELECT COUNT(*) FROM seen"),
                "inbox_pending": count("SELECT COUNT(*) FROM inbox WHERE delivered_at IS NULL"),
                "wakes": dict(db.execute("SELECT status, COUNT(*) FROM wakes GROUP BY status").fetchall()),
                "sent": dict(db.execute("SELECT status, COUNT(*) FROM outbox GROUP BY status").fetchall()),
                "incomplete_multipart": count(
                    "SELECT COUNT(*) FROM (SELECT msg_id FROM parts GROUP BY msg_id HAVING COUNT(*) < MAX(total))"),
            }
