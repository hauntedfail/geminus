"""Unit tests for geminish OTA updates and owner-delegated cross-device execution."""
from __future__ import annotations

import dataclasses
import hashlib
import io
import json
from pathlib import Path
import sys
import tarfile
import tempfile
import time
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))

from geminish import bridge, client, config, ota, policy, protocol  # noqa: E402
from geminish.config import Config, ConfigError  # noqa: E402
from geminish.store import Store  # noqa: E402


class _FakeMailbox:
    def __init__(self):
        self.messages: list[dict] = []
        self._seq = 0

    def post(self, peer_id: str, content: str, metadata: dict) -> dict:
        self._seq += 1
        item = {"id": f"h-{self._seq}", "peer_id": peer_id, "content": content, "metadata": metadata}
        self.messages.append(item)
        return item

    def recent(self, *, page: int = 1, size: int = 50) -> list[dict]:
        if page != 1:
            return []
        return list(reversed(self.messages))

    def peer_metadata(self, timeout=None) -> dict:
        return {}


def _make_cfg(me: str, tmp: Path, secret: bytes) -> Config:
    members = policy.Membership((
        policy.Member("h1karu", "human"),
        policy.Member("chrome", "agent"),
        policy.Member("sibyl", "agent"),
        policy.Member("mirai", "agent"),
    ))
    return Config(
        me=me,
        membership=members,
        session="geminise-main",
        base_url="https://honcho.haunted.fail",
        api_key="test",
        workspace="sibyl",
        secret=secret,
        state_dir=tmp / me,
        hermes_bin="/usr/bin/true",
        push=False,
    )


class TestOTA(unittest.TestCase):
    def test_version_compare(self):
        self.assertTrue(ota.is_newer("0.8.0", "0.9.0"))
        self.assertTrue(ota.is_newer("0.9.0-dev", "0.9.0"))
        self.assertFalse(ota.is_newer("0.9.0", "0.9.0"))
        self.assertFalse(ota.is_newer("0.10.0", "0.9.0"))

    def test_safe_extract_and_install(self):
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w:gz") as tf:
            for name, body in (
                ("geminish/plugin.yaml", "name: geminish\nversion: 0.9.0\n"),
                ("geminish/__init__.py", '__version__ = "0.9.0"\n'),
                ("geminish/cli.py", "#!/usr/bin/env python3\n"),
            ):
                data = body.encode("utf-8")
                info = tarfile.TarInfo(name)
                info.size = len(data)
                tf.addfile(info, io.BytesIO(data))
        archive = buf.getvalue()

        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            extracted = ota._safe_extract_bundle(archive, root / "stage", "0.9.0")
            self.assertTrue((extracted / "plugin.yaml").is_file())

            target = root / "plugins" / "geminish"
            target.mkdir(parents=True)
            (target / "old.py").write_text("old", encoding="utf-8")
            backup = ota._install_extracted(extracted, target, "0.8.0")
            self.assertTrue((backup / "old.py").is_file())
            self.assertTrue((target / "__init__.py").is_file())
            self.assertFalse((target / "old.py").exists())

    def test_safe_extract_rejects_traversal(self):
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w:gz") as tf:
            info = tarfile.TarInfo("geminish/../evil.py")
            data = b"evil"
            info.size = len(data)
            tf.addfile(info, io.BytesIO(data))
        with tempfile.TemporaryDirectory() as td:
            with self.assertRaises(ota.OTAError):
                ota._safe_extract_bundle(buf.getvalue(), Path(td), "0.9.0")


class TestDelegate(unittest.TestCase):
    def test_delegate_roundtrip_and_blocks_peer_escalation(self):
        secret = hashlib.sha256(b"test-group-secret").digest()
        with tempfile.TemporaryDirectory() as td:
            root = Path(td)
            chrome_cfg = _make_cfg("chrome", root, secret)
            mirai_cfg = _make_cfg("mirai", root, secret)
            chrome_cfg.state_dir.mkdir(parents=True)
            mirai_cfg.state_dir.mkdir(parents=True)
            chrome_store = Store(chrome_cfg.store_path)
            mirai_store = Store(mirai_cfg.store_path)
            mb = _FakeMailbox()

            # Baseline both stores.
            client.sync(chrome_cfg, chrome_store, mb)
            client.sync(mirai_cfg, mirai_store, mb)

            # Peer-woken turn MUST NOT be allowed to initiate a delegated task.
            with self.assertRaises(client.SendError):
                client.send(chrome_cfg, chrome_store, mb, "do stuff", to="mirai",
                            kind=client.DELEGATE_KIND, provenance="agent_reply", delegate=True)

            # Human-initiated turn sends delegated task.
            sent = client.send(chrome_cfg, chrome_store, mb, "check hostname", to="mirai",
                               kind=client.DELEGATE_KIND, stage="decided", expects_reply="requested",
                               notify="mention", provenance="initiated", delegate=True)
            self.assertTrue(sent["ok"])
            self.assertTrue(sent["delegated"])
            ref = sent["ref"]

            # Mirai ingests and sees Wake(delegated=True).
            wakes = client.sync(mirai_cfg, mirai_store, mb)
            self.assertEqual(len(wakes), 1)
            self.assertTrue(wakes[0].delegated)
            prompt = client.wake_prompt(mirai_cfg, wakes[0].envelope, delegated=True)
            self.assertIn("owner-delegated instruction from h1karu", prompt)

            # Mirai executes and automatically returns delegate_result.
            ok, detail = client.run_delegated_turn(
                mirai_cfg, mirai_store, mb, wakes[0],
                executor=lambda _cfg, _w: (True, "turn completed", "MIRAI_HOST_OK"),
            )
            self.assertTrue(ok)
            self.assertEqual(detail, "turn completed")

            # Chrome receives delegate_result in its inbox on the same ref.
            client.sync(chrome_cfg, chrome_store, mb, inline=True)
            inbox = chrome_store.take_inbox(ref=ref)
            self.assertEqual(len(inbox), 1)
            env = protocol.parse(inbox[0]["wire"])
            self.assertEqual(dict(env.metadata)["kind"], client.DELEGATE_RESULT_KIND)
            payload = json.loads(env.text)
            self.assertTrue(payload["ok"])
            self.assertEqual(payload["output"], "MIRAI_HOST_OK")


class TestHumanPubkeys(unittest.TestCase):
    def test_parse_single_and_multiple_pubkeys(self):
        k1 = "11" * 32
        k2 = "22" * 32
        single = config._human_pubkeys({"h1karu": k1})
        self.assertEqual(single["h1karu"], (bytes.fromhex(k1),))
        multi = config._human_pubkeys({"h1karu": [k1, k2]})
        self.assertEqual(multi["h1karu"], (bytes.fromhex(k1), bytes.fromhex(k2)))
        with self.assertRaises(ConfigError):
            config._human_pubkeys({"h1karu": []})
        with self.assertRaises(ConfigError):
            config._human_pubkeys({"h1karu": ["short"]})

    def test_bridge_verifies_any_configured_pubkey(self):
        secret = hashlib.sha256(b"test-group-secret").digest()
        k1 = bytes([0x11] * 32)
        k2 = bytes([0x22] * 32)
        with tempfile.TemporaryDirectory() as td:
            cfg = dataclasses.replace(_make_cfg("mirai", Path(td), secret), human_pubkeys={"h1karu": (k1, k2)})
            cfg.state_dir.mkdir(parents=True)
            b = bridge.Bridge(cfg, Store(cfg.store_path), _FakeMailbox())  # type: ignore[arg-type]
            orig = bridge.verify_human_ed25519
            try:
                bridge.verify_human_ed25519 = lambda pub, _c, _l, _t, sig: pub == k2 and sig == "ok-sig"
                ok, err = b._verify_human_prompt("h1karu", "hi", "lbl", 123, {"sig_alg": "ed25519", "sig": "ok-sig"})
                self.assertTrue(ok, err)
                ok2, _ = b._verify_human_prompt("h1karu", "hi", "lbl", 123, {"sig_alg": "ed25519", "sig": "bad-sig"})
                self.assertFalse(ok2)
            finally:
                bridge.verify_human_ed25519 = orig


class TestBridgeModelDiscovery(unittest.TestCase):
    def test_split_hermes_provider_and_preludes(self):
        providers = {"sibyl", "magpie", "custom:near-ai", "openai-codex"}
        self.assertEqual(bridge.split_hermes_provider("sibyl:group/lvmh", providers), ("sibyl", "group/lvmh"))
        self.assertEqual(
            bridge.split_hermes_provider("custom:near-ai:deepseek-ai/DeepSeek-V4", providers),
            ("custom:near-ai", "deepseek-ai/DeepSeek-V4"),
        )
        # Colon in a model tag (not a known provider) stays untouched.
        self.assertEqual(bridge.split_hermes_provider("llama3:8b", providers), ("", "llama3:8b"))
        self.assertEqual(bridge.split_hermes_provider("group/lvmh", providers), ("", "group/lvmh"))

        hermes = next(h for h in bridge.HARNESSES if h.id == "hermes")
        self.assertEqual(
            hermes.preludes({"model": "sibyl:group/miumiu", "effort": ""}, {}, hermes_providers=providers),
            [("model", "/model group/miumiu --provider sibyl")],
        )
        self.assertEqual(
            hermes.preludes({"model": "custom:near-ai:z-ai/glm", "effort": ""}, {}, hermes_providers=providers),
            [("model", "/model z-ai/glm --provider custom:near-ai")],
        )
        self.assertEqual(
            hermes.preludes({"model": "group/lvmh", "effort": ""}, {}, hermes_providers=providers),
            [("model", "/model group/lvmh")],
        )

    def test_format_inventory_models_prioritizes_current_and_default(self):
        data = {
            "default_provider": "sibyl",
            "default_model": "group/lvmh",
            "providers": [
                {"slug": "openai-codex", "is_current": False, "is_user_defined": False, "models": ["gpt-6.2-astra"], "aliases": []},
                {"slug": "magpie", "is_current": False, "is_user_defined": True, "models": ["group/vvx"], "aliases": ["custom:magpie"]},
                {"slug": "sibyl", "is_current": True, "is_user_defined": True, "models": ["group/yvng", "group/lvmh", "bad model"], "aliases": ["custom:sibyl"]},
            ],
        }
        models, providers = bridge._format_inventory_models(data)
        self.assertEqual(models, ["sibyl:group/lvmh", "sibyl:group/yvng", "magpie:group/vvx", "openai-codex:gpt-6.2-astra"])
        self.assertIn("sibyl", providers)
        self.assertIn("custom:magpie", providers)

    def test_config_fallback_reads_providers_and_default(self):
        yaml_text = (
            "model:\n"
            "  default: group/lvmh\n"
            "  provider: sibyl\n"
            "providers:\n"
            "  sibyl:\n"
            "    default_model: group/yvng\n"
            "    models:\n"
            "      group/yvng:\n"
            "        context_length: 1000000\n"
            "      group/lvmh:\n"
            "        context_length: 1048576\n"
            "  magpie:\n"
            "    models:\n"
            "      - group/vvx\n"
        )
        with tempfile.TemporaryDirectory() as td:
            cfg_path = Path(td) / "config.yaml"
            cfg_path.write_text(yaml_text, encoding="utf-8")
            models, providers = bridge._discover_hermes_models_from_config(cfg_path)
        self.assertEqual(models[:3], ["sibyl:group/lvmh", "sibyl:group/yvng", "magpie:group/vvx"])
        self.assertEqual(providers, {"sibyl", "magpie"})

    def test_node_card_includes_models_without_breaking_sigs(self):
        secret = hashlib.sha256(b"test-group-secret").digest()
        hermes = next(h for h in bridge.HARNESSES if h.id == "hermes")
        card_without = bridge.node_card(secret, "chrome", [hermes], 1700000000.0, "/tmp")
        card_with = bridge.node_card(
            secret, "chrome", [hermes], 1700000000.0, "/tmp",
            models={"hermes": ["sibyl:group/lvmh", "sibyl:group/yvng"]},
        )
        self.assertEqual(card_with["sig"], card_without["sig"])
        self.assertEqual(card_with["options_sig"], card_without["options_sig"])
        self.assertEqual(card_with["cwd_sig"], card_without["cwd_sig"])
        self.assertEqual(card_with["harnesses"][0]["models"], ["sibyl:group/lvmh", "sibyl:group/yvng"])


if __name__ == "__main__":
    unittest.main()
