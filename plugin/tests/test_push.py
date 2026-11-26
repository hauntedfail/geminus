"""Push doorbell behaviour. No network: a scripted wire and a fake HTTP port stand in for Mozilla."""
from __future__ import annotations

import base64
import hashlib
import importlib
import json
import os
from pathlib import Path
import socket
import struct
import sys
import threading

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
sys.path.insert(0, str(Path(__file__).resolve().parent))

from geminish import cli, client, mailbox, push  # noqa: E402
from test_geminish import SECRET, FakeHoncho, make, net, say  # noqa: E402,F401

KEY = bytes.fromhex(SECRET)
EP = {name: f"https://updates.push.services.mozilla.com/wpush/v1/{name}-token" for name in ("chrome", "sibyl", "mirai")}
NOW = 1_794_500_000


def publish(honcho, peer, *, secret=KEY, endpoint=None):
    honcho.set_peer_metadata(peer, push.META_KEY, push.record(secret, peer, endpoint or EP[peer], NOW))


@pytest.fixture
def posts(monkeypatch):
    """Every doorbell POST made by a send, with how many messages Honcho held at that moment."""
    calls: list[str] = []
    monkeypatch.setattr(push, "http_post", lambda url, timeout: calls.append(url) or 201)
    return calls


# ----- the published endpoint record ---------------------------------------------

def test_endpoint_record_is_accepted_only_for_the_peer_that_signed_it():
    value = push.record(KEY, "sibyl", EP["sibyl"], NOW)
    assert push.endpoint_from(KEY, "sibyl", value) == EP["sibyl"]
    assert push.endpoint_from(KEY, "mirai", value) is None
    assert push.endpoint_from(b"x" * 32, "sibyl", value) is None
    assert push.endpoint_from(KEY, "sibyl", {**value, "endpoint": EP["mirai"]}) is None
    assert push.endpoint_from(KEY, "sibyl", None) is None
    assert push.endpoint_from(KEY, "sibyl", "not a record") is None


@pytest.mark.parametrize("url", [
    "http://updates.push.services.mozilla.com/wpush/v1/t",
    "https://updates.push.services.mozilla.com.evil.example/wpush/v1/t",
    "https://user@updates.push.services.mozilla.com/wpush/v1/t",
    "https://updates.push.services.mozilla.com:8443/wpush/v1/t",
    "https://updates.push.services.mozilla.com/other/t",
    "https://honcho.invalid/wpush/v1/t",
    "",
])
def test_only_mozilla_push_endpoints_are_ever_rung(url):
    assert not push.valid_endpoint(url)
    assert push.endpoint_from(KEY, "sibyl", push.record(KEY, "sibyl", url, NOW)) is None


def test_mozilla_endpoint_is_valid():
    assert push.valid_endpoint(EP["sibyl"])


# ----- ringing on send -----------------------------------------------------------

def test_new_mention_rings_only_the_named_recipient_after_it_is_recorded(net, monkeypatch):
    honcho, _hosts = net
    publish(honcho, "sibyl")
    publish(honcho, "mirai")
    calls = []
    monkeypatch.setattr(push, "http_post", lambda url, timeout: calls.append((url, len(honcho.messages))) or 201)
    result = say(net, "chrome", "hi", to="sibyl")
    assert calls == [(EP["sibyl"], 1)]  # the mail was already stored when the bell rang
    assert result["ok"] and result["rang"] == ["sibyl"]


def test_all_rings_every_other_agent(net, posts):
    honcho, _hosts = net
    for peer in ("chrome", "sibyl", "mirai"):
        publish(honcho, peer)
    result = say(net, "chrome", "notice", to="@all", notify="all")
    assert sorted(posts) == sorted([EP["sibyl"], EP["mirai"]])
    assert sorted(result["rang"]) == ["mirai", "sibyl"]


def test_digest_and_replies_never_ring(net, posts):
    honcho, _hosts = net
    publish(honcho, "sibyl")
    assert say(net, "chrome", "later", to="sibyl", notify="digest")["ok"]
    assert say(net, "chrome", "answer", to="sibyl", provenance="agent_reply")["ok"]
    assert posts == []


@pytest.mark.parametrize("outcome", [OSError("down"), 410, 500])
def test_a_failed_ring_never_fails_the_send(net, monkeypatch, outcome):
    honcho, _hosts = net
    publish(honcho, "sibyl")

    def post(url, timeout):
        if isinstance(outcome, BaseException):
            raise outcome
        return outcome
    monkeypatch.setattr(push, "http_post", post)
    result = say(net, "chrome", "hi", to="sibyl")
    assert result["ok"] and result["recorded"] == 1 and result["rang"] == []


def test_recipient_without_a_published_endpoint_is_left_to_its_poller(net, posts):
    result = say(net, "chrome", "hi", to="sibyl")
    assert result["ok"] and result["rang"] == [] and posts == []


def test_endpoint_record_signed_with_another_secret_is_not_rung(net, posts):
    honcho, _hosts = net
    publish(honcho, "sibyl", secret=b"z" * 32)
    assert say(net, "chrome", "hi", to="sibyl")["rang"] == []
    assert posts == []


def test_unreadable_directory_never_fails_the_send(net, posts, monkeypatch):
    honcho, _hosts = net
    monkeypatch.setattr(honcho, "peer_metadata",
                        lambda timeout=None: (_ for _ in ()).throw(mailbox.MailboxError("down")))
    result = say(net, "chrome", "hi", to="sibyl")
    assert result["ok"] and result["rang"] == []


def test_push_disabled_host_neither_reads_the_directory_nor_rings(tmp_path, posts):
    honcho = FakeHoncho()
    cfg, store = make(tmp_path, "chrome", push=False)
    assert cfg.push is False
    client.sync(cfg, store, honcho)
    publish(honcho, "sibyl")
    honcho.peer_metadata = lambda timeout=None: pytest.fail("directory must not be read")
    result = client.send(cfg, store, honcho, "hi", to="sibyl")
    assert result["ok"] and "rang" not in result and posts == []


def test_push_is_on_by_default(tmp_path):
    cfg, _store = make(tmp_path, "chrome")
    assert cfg.push is True


# ----- the listener --------------------------------------------------------------

class Wire:
    """A scripted AutoPush connection: records what the client sends, replays what the server says."""

    def __init__(self, *replies):
        self.sent: list[dict] = []
        self.replies = list(replies)
        self.closed = False

    def send(self, text):
        self.sent.append(json.loads(text))

    def recv(self, timeout):
        if not self.replies:
            raise push.PushError("script ended")
        item = self.replies.pop(0)
        if isinstance(item, BaseException):
            raise item
        return json.dumps(item)

    def close(self):
        self.closed = True


def listener(tmp_path, honcho, wire, rings, **extra):
    cfg, store = make(tmp_path, "chrome")
    ids = iter(["chan-1", "chan-2", "chan-3"])
    return push.Listener(me=cfg.me, secret=cfg.secret, store=store, directory=honcho,
                         on_ring=lambda: rings.append(1), connect=lambda: wire,
                         clock=lambda: NOW, new_id=lambda: next(ids), **extra), store


def hello(uaid="u-1"):
    return {"messageType": "hello", "status": 200, "uaid": uaid, "use_webpush": True}


def registered(channel="chan-1", endpoint=None):
    return {"messageType": "register", "status": 200, "channelID": channel,
            "pushEndpoint": endpoint or EP["chrome"]}


def test_first_connection_registers_and_publishes_a_signed_endpoint(tmp_path):
    honcho, rings = FakeHoncho(), []
    wire = Wire(hello(), registered())
    lst, store = listener(tmp_path, honcho, wire, rings)
    with pytest.raises(push.PushError, match="script ended"):
        lst.session(threading.Event())
    assert wire.sent[0] == {"messageType": "hello", "use_webpush": True, "channelIDs": []}
    assert wire.sent[1] == {"messageType": "register", "channelID": "chan-1"}
    assert push.endpoint_from(KEY, "chrome", honcho.peer_metadata()["chrome"][push.META_KEY]) == EP["chrome"]
    assert (store.get_meta("push_uaid"), store.get_meta("push_channel"), store.get_meta("push_endpoint")) == (
        "u-1", "chan-1", EP["chrome"])
    assert wire.closed and rings == []


def test_notification_is_acknowledged_and_rings_once(tmp_path):
    honcho, rings = FakeHoncho(), []
    wire = Wire(hello(), registered(), {"messageType": "notification", "channelID": "chan-1", "version": "v9"})
    lst, _store = listener(tmp_path, honcho, wire, rings)
    with pytest.raises(push.PushError):
        lst.session(threading.Event())
    assert wire.sent[-1] == {"messageType": "ack", "updates": [{"channelID": "chan-1", "version": "v9", "code": 100}]}
    assert rings == [1]


def test_reconnect_keeps_the_same_endpoint_and_does_not_republish(tmp_path):
    honcho, rings = FakeHoncho(), []
    lst, _store = listener(tmp_path, honcho, Wire(hello(), registered()), rings)
    with pytest.raises(push.PushError):
        lst.session(threading.Event())
    writes = []
    honcho.set_peer_metadata = lambda *args: writes.append(args)
    again = Wire(hello())
    lst._connect = lambda: again
    with pytest.raises(push.PushError):
        lst.session(threading.Event())
    assert again.sent == [{"messageType": "hello", "use_webpush": True, "uaid": "u-1", "channelIDs": ["chan-1"]}]
    assert writes == []


def test_a_new_uaid_from_the_server_registers_and_publishes_again(tmp_path):
    honcho, rings = FakeHoncho(), []
    lst, store = listener(tmp_path, honcho, Wire(hello(), registered()), rings)
    with pytest.raises(push.PushError):
        lst.session(threading.Event())
    fresh = EP["chrome"] + "-2"
    again = Wire(hello("u-2"), registered("chan-2", fresh))
    lst._connect = lambda: again
    with pytest.raises(push.PushError):
        lst.session(threading.Event())
    assert again.sent[1] == {"messageType": "register", "channelID": "chan-2"}
    assert push.endpoint_from(KEY, "chrome", honcho.peer_metadata()["chrome"][push.META_KEY]) == fresh
    assert store.get_meta("push_uaid") == "u-2"


def test_notification_that_arrives_before_the_register_reply_still_rings(tmp_path):
    honcho, rings = FakeHoncho(), []
    wire = Wire(hello(), {"messageType": "notification", "channelID": "old", "version": "v1"}, registered())
    lst, _store = listener(tmp_path, honcho, wire, rings)
    with pytest.raises(push.PushError):
        lst.session(threading.Event())
    assert rings == [1] and push.META_KEY in honcho.peer_metadata()["chrome"]


@pytest.mark.parametrize("bad", [
    {"messageType": "hello", "status": 500, "uaid": "u-1"},
    {"messageType": "hello", "status": 200},
])
def test_refused_hello_ends_the_session_without_publishing(tmp_path, bad):
    honcho, rings = FakeHoncho(), []
    lst, _store = listener(tmp_path, honcho, Wire(bad), rings)
    with pytest.raises(push.PushError):
        lst.session(threading.Event())
    assert honcho.peer_metadata() == {}


def test_endpoint_outside_mozilla_is_refused_and_not_published(tmp_path):
    honcho, rings = FakeHoncho(), []
    wire = Wire(hello(), registered(endpoint="https://evil.example/wpush/v1/x"))
    lst, store = listener(tmp_path, honcho, wire, rings)
    with pytest.raises(push.PushError, match="endpoint"):
        lst.session(threading.Event())
    assert honcho.peer_metadata() == {} and store.get_meta("push_endpoint") is None


def test_quiet_connection_is_pinged_and_dropped_when_the_server_stays_silent(tmp_path):
    honcho, rings = FakeHoncho(), []
    wire = Wire(hello(), registered(), socket.timeout(), {}, socket.timeout(), socket.timeout())
    lst, _store = listener(tmp_path, honcho, wire, rings)
    with pytest.raises(socket.timeout):
        lst.session(threading.Event())
    assert wire.sent[2:] == [{}, {}]  # one ping answered, the second one not
    assert wire.closed


def test_run_reconnects_after_a_failure_and_stops_when_asked(tmp_path):
    honcho, rings = FakeHoncho(), []
    stop = threading.Event()
    wires = [Wire(hello(), registered()), Wire(hello(), {"messageType": "notification", "channelID": "chan-1",
                                                         "version": "v2"})]
    waits = []

    def connect():
        if not wires:
            stop.set()
            raise OSError("no more")
        return wires.pop(0)
    lst, store = listener(tmp_path, honcho, None, rings)
    lst._connect = connect
    lst.run(stop, wait=lambda seconds: waits.append(seconds))
    assert rings == [1] and len(waits) >= 2
    assert all(5 <= seconds <= 300 for seconds in waits)
    assert store.get_meta("push_state").startswith("down")


# ----- the WebSocket client, against a socket pair -------------------------------

GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"


def server_handshake(sock, *, accept=None):
    data = b""
    while b"\r\n\r\n" not in data:
        data += sock.recv(4096)
    headers = {k.strip().lower(): v.strip() for k, v in
               (line.split(":", 1) for line in data.decode().split("\r\n")[1:] if ":" in line)}
    assert headers["upgrade"].lower() == "websocket" and headers["sec-websocket-version"] == "13"
    key = headers["sec-websocket-key"]
    accept = accept or base64.b64encode(hashlib.sha1((key + GUID).encode()).digest()).decode()
    sock.sendall(f"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
                 f"Sec-WebSocket-Accept: {accept}\r\n\r\n".encode())


def frame(opcode, payload=b"", *, fin=True, masked=False):
    head = bytes([(0x80 if fin else 0) | opcode])
    n = len(payload)
    bit = 0x80 if masked else 0
    if n < 126:
        head += bytes([bit | n])
    elif n < 65536:
        head += bytes([bit | 126]) + struct.pack(">H", n)
    else:
        head += bytes([bit | 127]) + struct.pack(">Q", n)
    if masked:
        mask = b"\x01\x02\x03\x04"
        return head + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
    return head + payload


def read_exact(sock, n):
    data = b""
    while len(data) < n:
        chunk = sock.recv(n - len(data))
        assert chunk, "client closed"
        data += chunk
    return data


def read_client_frame(sock):
    b0, b1 = read_exact(sock, 2)
    assert b1 & 0x80, "client frames must be masked"
    n = b1 & 0x7F
    if n == 126:
        n = struct.unpack(">H", read_exact(sock, 2))[0]
    elif n == 127:
        n = struct.unpack(">Q", read_exact(sock, 8))[0]
    mask = read_exact(sock, 4)
    return b0 & 0x0F, bytes(b ^ mask[i % 4] for i, b in enumerate(read_exact(sock, n)))


@pytest.fixture
def pair():
    ours, theirs = socket.socketpair()
    theirs.settimeout(5)
    yield ours, theirs
    for sock in (ours, theirs):
        try:
            sock.close()
        except OSError:
            pass


def open_ws(pair, **kwargs):
    ours, theirs = pair
    thread = threading.Thread(target=server_handshake, args=(theirs,), kwargs=kwargs)
    thread.start()
    try:
        return push.WebSocket(ours, "push.invalid", "/", timeout=5)
    finally:
        thread.join(5)


def test_websocket_sends_masked_text_the_server_can_read(pair):
    ws = open_ws(pair)
    ws.send('{"messageType":"hello"}')
    ws.send("x" * 300)
    assert read_client_frame(pair[1]) == (0x1, b'{"messageType":"hello"}')
    assert read_client_frame(pair[1]) == (0x1, b"x" * 300)


def test_websocket_joins_fragments_and_reads_long_frames(pair):
    ws = open_ws(pair)
    data = frame(0x1, b'{"a":', fin=False) + frame(0x0, b'"b"}') + frame(0x1, b"y" * 70000)
    writer = threading.Thread(target=pair[1].sendall, args=(data,))  # larger than the socket buffer
    writer.start()
    assert ws.recv(5) == '{"a":"b"}'
    assert ws.recv(5) == "y" * 70000
    writer.join(5)


def test_websocket_answers_a_ping_and_keeps_reading(pair):
    ws = open_ws(pair)
    pair[1].sendall(frame(0x9, b"hb") + frame(0x1, b"{}"))
    assert ws.recv(5) == "{}"
    assert read_client_frame(pair[1]) == (0xA, b"hb")


def test_websocket_timeout_keeps_a_half_received_frame(pair):
    ws = open_ws(pair)
    whole = frame(0x1, b'{"messageType":"notification"}')
    pair[1].sendall(whole[:7])
    with pytest.raises(socket.timeout):
        ws.recv(0.05)
    pair[1].sendall(whole[7:])
    assert ws.recv(5) == '{"messageType":"notification"}'


def test_websocket_close_frame_and_dropped_connection_raise(pair):
    ws = open_ws(pair)
    pair[1].sendall(frame(0x8, struct.pack(">H", 1000)))
    with pytest.raises(push.PushError, match="closed"):
        ws.recv(5)
    other = socket.socketpair()
    thread = threading.Thread(target=server_handshake, args=(other[1],))
    thread.start()
    ws2 = push.WebSocket(other[0], "push.invalid", "/", timeout=5)
    thread.join(5)
    other[1].close()
    with pytest.raises(push.PushError, match="closed"):
        ws2.recv(5)
    other[0].close()


def test_websocket_refuses_a_wrong_accept_key_and_masked_server_frames(pair):
    with pytest.raises(push.PushError, match="handshake"):
        open_ws(pair, accept="bm90IHRoZSByaWdodCBrZXk=")
    other = socket.socketpair()
    thread = threading.Thread(target=server_handshake, args=(other[1],))
    thread.start()
    ws = push.WebSocket(other[0], "push.invalid", "/", timeout=5)
    thread.join(5)
    other[1].sendall(frame(0x1, b"{}", masked=True))
    with pytest.raises(push.PushError, match="masked"):
        ws.recv(5)
    for sock in other:
        sock.close()


def test_websocket_refuses_an_oversized_message(pair):
    ws = open_ws(pair)
    pair[1].sendall(bytes([0x81, 127]) + struct.pack(">Q", push.MAX_MESSAGE + 1))
    with pytest.raises(push.PushError, match="large"):
        ws.recv(5)


# ----- the poller reacts to a ring -----------------------------------------------

def test_poller_wait_ends_at_once_on_a_ring_and_clears_it():
    stop, ring = threading.Event(), threading.Event()
    ring.set()
    assert cli._wait(30.0, stop, ring) is True
    assert not ring.is_set()


def test_poller_wait_runs_its_full_delay_without_a_ring():
    stop, ring = threading.Event(), threading.Event()
    assert cli._wait(0.05, stop, ring) is False


def test_poller_wait_ends_when_asked_to_stop():
    stop, ring = threading.Event(), threading.Event()
    stop.set()
    assert cli._wait(30.0, stop, ring) is False


def test_one_shot_read_leaves_a_running_pollers_push_state_alone(tmp_path, monkeypatch):
    cfg, store = make(tmp_path, "chrome")
    store.set_meta("push_state", "connected since 1")
    started = []
    monkeypatch.setattr(cli, "_start_listener", lambda *args: started.append(args))
    cli._prepare_push(cfg, store, threading.Event(), threading.Event(), once=True)
    assert store.get_meta("push_state") == "connected since 1" and started == []
    cli._prepare_push(cfg, store, threading.Event(), threading.Event(), once=False)
    assert len(started) == 1 and store.get_meta("push_state") is None  # stale state from a dead poller is gone


def test_resident_poller_with_push_off_clears_the_state_and_starts_nothing(tmp_path, monkeypatch):
    cfg, store = make(tmp_path, "chrome", push=False)
    store.set_meta("push_state", "connected since 1")
    monkeypatch.setattr(cli, "_start_listener", lambda *args: pytest.fail("must not start"))
    cli._prepare_push(cfg, store, threading.Event(), threading.Event(), once=False)
    assert store.get_meta("push_state") is None


def test_rung_reads_keep_a_minimum_distance():
    assert cli._ring_gap(last_read=100.0, now=100.2) == pytest.approx(cli.RING_FLOOR - 0.2)
    assert cli._ring_gap(last_read=100.0, now=100.0 + cli.RING_FLOOR + 3) == 0.0


def test_status_marks_a_push_state_left_behind_by_a_dead_poller(tmp_path):
    cfg, store = make(tmp_path, "chrome")
    store.set_meta("push_endpoint", EP["chrome"])
    store.set_meta("push_state", "connected since 1")
    store.touch_sync(now=1000.0)
    fresh = cli._push_status(cfg, store, now=1005.0)
    assert fresh == {"enabled": True, "registered": True, "state": "connected since 1"}
    stale = cli._push_status(cfg, store, now=5000.0)
    assert "no mailbox read" in stale["state"]
    assert EP["chrome"] not in json.dumps(stale)


def test_mail_signed_under_a_label_that_is_not_a_mail_provenance_is_rejected(net):
    honcho, hosts = net
    cfg, _store = hosts["sibyl"]
    wire = client.protocol.serialize(client.protocol.build_envelope({
        "id": "11111111-1111-4111-8111-111111111111", "from": "sibyl", "to": "@chrome", "kind": "question",
        "stage": "done", "expects_reply": "none", "notify": "mention", "status": "open",
        "session": cfg.session, "ref": "sibyl-x"}, "hi"))
    label = push.LABEL + "sibyl"
    import time
    stamp = int(time.time())
    honcho.post("sibyl", wire, {"geminise": {"provenance": label, "ts": stamp,
                                             "sig": mailbox.sign(cfg.secret, wire, label, stamp)}})
    chrome_cfg, chrome_store = hosts["chrome"]
    assert client.sync(chrome_cfg, chrome_store, honcho) == []
    assert chrome_store.handled("11111111-1111-4111-8111-111111111111") is None


# ----- Honcho peer metadata ------------------------------------------------------

def test_publishing_keeps_other_metadata_of_the_same_peer(monkeypatch):
    box = mailbox.Mailbox("https://honcho.invalid", "k", "w", "geminise-main")
    calls = []

    def call(method, path, body=None, *, write=False):
        calls.append((method, path, body))
        if path.endswith("/peers/list?page=1&size=100"):
            return {"items": [{"id": "chrome", "metadata": {"other": 1}}, {"id": "sibyl", "metadata": {}}]}
        return {}
    monkeypatch.setattr(box, "_call", call)
    assert box.peer_metadata() == {"chrome": {"other": 1}, "sibyl": {}}
    box.set_peer_metadata("chrome", push.META_KEY, {"v": 1})
    assert calls[-1] == ("PUT", "/v3/workspaces/w/peers/chrome", {"metadata": {"other": 1, push.META_KEY: {"v": 1}}})


def paged_box(monkeypatch, pages):
    box = mailbox.Mailbox("https://honcho.invalid", "k", "w", "geminise-main")
    calls = []

    def call(method, path, body=None, *, write=False, timeout=None):
        calls.append((method, path, body))
        if "/peers/list?page=" in path:
            number = int(path.split("page=")[1].split("&")[0])
            return {"items": pages[number - 1] if number <= len(pages) else [], "pages": len(pages)}
        return {}
    monkeypatch.setattr(box, "_call", call)
    return box, calls


def test_peer_directory_is_read_across_pages(monkeypatch):
    first = [{"id": f"p{i}", "metadata": {}} for i in range(100)]
    box, _calls = paged_box(monkeypatch, [first, [{"id": "chrome", "metadata": {"other": 1}}]])
    assert box.peer_metadata()["chrome"] == {"other": 1}
    box.set_peer_metadata("chrome", push.META_KEY, {"v": 1})
    assert _calls[-1][2] == {"metadata": {"other": 1, push.META_KEY: {"v": 1}}}


def test_publishing_for_a_peer_missing_from_the_directory_writes_nothing(monkeypatch):
    box, calls = paged_box(monkeypatch, [[{"id": "sibyl", "metadata": {"keep": 1}}]])
    with pytest.raises(mailbox.MailboxError, match="not found"):
        box.set_peer_metadata("chrome", push.META_KEY, {"v": 1})
    assert all(method != "PUT" for method, _path, _body in calls)


def test_directory_read_for_a_ring_uses_a_short_timeout(net, posts):
    honcho, _hosts = net
    seen = []
    honcho.peer_metadata = lambda timeout=None: seen.append(timeout) or {}
    say(net, "chrome", "hi", to="sibyl")
    assert seen == [push.RING_TIMEOUT]


@pytest.mark.parametrize("slow", ["directory", "post"])
def test_a_slow_ring_does_not_hold_up_the_send(net, monkeypatch, slow):
    import time
    honcho, _hosts = net
    publish(honcho, "sibyl")
    release = threading.Event()
    monkeypatch.setattr(push, "RING_DEADLINE", 0.2)
    if slow == "directory":
        records = honcho.peer_metadata()
        honcho.peer_metadata = lambda timeout=None: release.wait(10) and records
    monkeypatch.setattr(push, "http_post", lambda url, timeout: (release.wait(10) if slow == "post" else True) and 201)
    started = time.monotonic()
    result = say(net, "chrome", "hi", to="sibyl")
    elapsed = time.monotonic() - started
    release.set()
    assert result["ok"] and result["recorded"] == 1 and result["rang"] == []
    assert elapsed < 2.0


# ----- upgrading an installed plugin ----------------------------------------------

def test_upgrade_replaces_the_plugin_and_keeps_the_identity_file(tmp_path, capsys):
    install = importlib.import_module("install")
    home = tmp_path / "hermes"
    old = home / "plugins" / "geminish"
    old.mkdir(parents=True)
    (old / "stale.py").write_text("old")
    (home / "honcho.json").write_text("{}")
    identity = home / "geminise.json"
    identity.write_text('{"from": "mirai", "secret": "s3cr3t-value"}')
    assert install.main(["--upgrade", "--hermes-home", str(home)]) == 0
    assert identity.read_text() == '{"from": "mirai", "secret": "s3cr3t-value"}'
    assert not (old / "stale.py").exists() and (old / "push.py").is_file() and (old / "cli.py").is_file()
    assert "s3cr3t-value" not in capsys.readouterr().out


def test_upgrade_carries_over_an_identity_file_kept_inside_the_plugin(tmp_path):
    install = importlib.import_module("install")
    home = tmp_path / "hermes"
    old = home / "plugins" / "geminish"
    old.mkdir(parents=True)
    (home / "honcho.json").write_text("{}")
    (old / "geminise.json").write_text('{"from": "sibyl"}')
    assert install.main(["--upgrade", "--hermes-home", str(home)]) == 0
    assert (old / "geminise.json").read_text() == '{"from": "sibyl"}' and (old / "push.py").is_file()


def test_upgrade_that_cannot_swap_directories_leaves_the_old_install_working(tmp_path, monkeypatch):
    install = importlib.import_module("install")
    home = tmp_path / "hermes"
    old = home / "plugins" / "geminish"
    old.mkdir(parents=True)
    (old / "cli.py").write_text("old version")
    (old / "geminise.json").write_text('{"from": "sibyl"}')
    (home / "honcho.json").write_text("{}")
    real = Path.rename

    def rename(self, target):
        if self.name == install.STAGING:
            raise PermissionError("file in use")
        return real(self, target)
    monkeypatch.setattr(Path, "rename", rename)
    assert install.main(["--upgrade", "--hermes-home", str(home)]) == 1
    assert (old / "cli.py").read_text() == "old version"
    assert (old / "geminise.json").read_text() == '{"from": "sibyl"}'
    assert sorted(path.name for path in (home / "plugins").iterdir()) == ["geminish"]


def test_upgrade_without_an_existing_install_is_refused(tmp_path):
    install = importlib.import_module("install")
    home = tmp_path / "hermes"
    home.mkdir()
    (home / "honcho.json").write_text("{}")
    assert install.main(["--upgrade", "--hermes-home", str(home)]) == 1
    assert not (home / "plugins").exists()


def test_fresh_install_still_needs_an_identity_and_a_secret(tmp_path):
    install = importlib.import_module("install")
    with pytest.raises(SystemExit):
        install.main(["--hermes-home", str(tmp_path)])
