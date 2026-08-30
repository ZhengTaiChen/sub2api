import importlib.util
import json
import sqlite3
import tempfile
from unittest import mock
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
MODULE_PATH = ROOT / "deploy" / "ops" / "new-api-channel-watchdog.py"
SPEC = importlib.util.spec_from_file_location("new_api_channel_watchdog", MODULE_PATH)
WATCHDOG = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(WATCHDOG)


class WatchdogTest(unittest.TestCase):
    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory()
        self.db_path = Path(self.temp_dir.name) / "one-api.db"
        connection = sqlite3.connect(self.db_path)
        connection.executescript(
            """
            CREATE TABLE channels (
                id INTEGER PRIMARY KEY,
                name TEXT NOT NULL,
                status INTEGER NOT NULL,
                base_url TEXT,
                setting TEXT,
                test_time INTEGER NOT NULL DEFAULT 0,
                response_time INTEGER NOT NULL DEFAULT 0
            );
            CREATE TABLE abilities (
                channel_id INTEGER NOT NULL,
                enabled INTEGER NOT NULL
            );
            CREATE TABLE logs (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                channel_id INTEGER,
                content TEXT,
                other TEXT,
                type INTEGER,
                created_at INTEGER,
                use_time INTEGER
            );
            CREATE TABLE options (
                key TEXT PRIMARY KEY,
                value TEXT NOT NULL
            );
            """
        )
        connection.commit()
        connection.close()

    def tearDown(self):
        self.temp_dir.cleanup()

    def connect(self):
        connection = sqlite3.connect(self.db_path)
        connection.row_factory = sqlite3.Row
        return connection

    def test_error_classification_excludes_client_cancellation(self):
        self.assertTrue(WATCHDOG.error_is_auth_or_balance("status_code=403, insufficient account balance"))
        self.assertTrue(WATCHDOG.error_is_auth_or_balance("余额不足"))
        self.assertTrue(WATCHDOG.error_is_auth_or_balance("insufficient_user_quota"))
        self.assertFalse(WATCHDOG.error_is_auth_or_balance("status_code=401, model_not_found"))
        self.assertFalse(WATCHDOG.error_is_auth_or_balance("No available channel for model"))
        self.assertFalse(WATCHDOG.error_is_auth_or_balance("route not found, status_code=401"))
        self.assertFalse(WATCHDOG.error_is_auth_or_balance("status_code=500, not implemented"))
        self.assertTrue(WATCHDOG.error_is_transient("status_code=524, gateway timeout"))
        self.assertFalse(WATCHDOG.error_is_transient("context canceled after timeout"))
        self.assertFalse(WATCHDOG.error_is_transient("client disconnected; broken pipe"))
        self.assertFalse(WATCHDOG.error_is_transient("model not supported after gateway timeout"))
        self.assertFalse(WATCHDOG.error_is_transient("status_code=500, unsupported endpoint"))

    def test_channel_state_backup_contains_only_governance_state(self):
        connection = self.connect()
        connection.execute("INSERT INTO channels(id, name, status) VALUES (26, 'low balance', 3)")
        connection.execute("INSERT INTO channels(id, name, status) VALUES (63, 'not implemented', 1)")
        connection.commit()
        channels = WATCHDOG.load_channels(connection)
        connection.close()

        state_path = Path(self.temp_dir.name) / "state.json"
        state_path.write_text('{"initialized": true, "statuses": {"26": 3}}\n', encoding="utf-8")
        backup_dir = Path(self.temp_dir.name) / "backups"
        backup = WATCHDOG.backup_channel_state(
            state_path,
            backup_dir,
            channels,
            {"initialized": True, "statuses": {"26": 3}},
            "before-test",
        )

        self.assertTrue(backup.is_file())
        self.assertEqual(0o600, backup.stat().st_mode & 0o777)
        payload = json.loads(backup.read_text(encoding="utf-8"))
        self.assertEqual({"26": 3, "63": 1}, payload["statuses"])
        self.assertEqual({"initialized": True, "statuses": {"26": 3}}, payload["watchdog_state"])
        self.assertEqual(
            {"captured_at", "source", "statuses", "watchdog_state"},
            set(payload),
        )

    def test_isolate_channels_disables_abilities_in_one_transaction(self):
        connection = self.connect()
        connection.execute("INSERT INTO channels(id, name, status) VALUES (63, 'bad', 1)")
        connection.execute("INSERT INTO abilities(channel_id, enabled) VALUES (63, 1)")
        connection.commit()
        connection.close()

        changed = WATCHDOG.isolate_channels(self.db_path, [63], False)

        self.assertEqual([63], changed)
        connection = self.connect()
        self.assertEqual(3, connection.execute("SELECT status FROM channels WHERE id = 63").fetchone()[0])
        self.assertEqual(0, connection.execute("SELECT enabled FROM abilities WHERE channel_id = 63").fetchone()[0])
        connection.close()

    def test_governance_preserves_keywords_and_disables_explicit_channels(self):
        connection = self.connect()
        connection.execute("INSERT INTO channels(id, name, status) VALUES (26, 'low balance', 1)")
        connection.execute("INSERT INTO abilities(channel_id, enabled) VALUES (26, 1)")
        connection.execute(
            "INSERT INTO options(key, value) VALUES (?, ?)",
            ("AutomaticDisableKeywords", "existing phrase"),
        )
        connection.commit()
        connection.close()

        changed, option_names = WATCHDOG.set_options_and_initial_isolation(
            self.db_path,
            [26],
            False,
            45.0,
        )

        self.assertEqual([26], changed)
        self.assertIn("AutomaticEnableChannelEnabled", option_names)
        connection = self.connect()
        values = dict(connection.execute("SELECT key, value FROM options"))
        self.assertEqual("false", values["AutomaticEnableChannelEnabled"])
        self.assertEqual("false", values["AutomaticDisableChannelEnabled"])
        self.assertEqual("45", values["ChannelDisableThreshold"])
        self.assertIn("existing phrase", values["AutomaticDisableKeywords"])
        self.assertEqual(3, connection.execute("SELECT status FROM channels WHERE id = 26").fetchone()[0])
        self.assertEqual(0, connection.execute("SELECT enabled FROM abilities WHERE channel_id = 26").fetchone()[0])
        connection.close()

    def test_dry_run_event_does_not_notify(self):
        logger = mock.Mock()

        with mock.patch.object(WATCHDOG, "notify") as notify:
            WATCHDOG.emit_event(
                {},
                logger,
                True,
                "warning",
                "new_api_channel_isolated",
                "channel #63 would be isolated",
            )

        notify.assert_not_called()
        logger.info.assert_called_once()

    def test_dry_run_does_not_write_sqlite(self):
        connection = self.connect()
        connection.execute("INSERT INTO channels(id, name, status) VALUES (26, 'low balance', 1)")
        connection.execute("INSERT INTO abilities(channel_id, enabled) VALUES (26, 1)")
        connection.commit()
        connection.close()

        changed, _ = WATCHDOG.set_options_and_initial_isolation(self.db_path, [26], True, 45.0)
        self.assertEqual([26], changed)
        connection = self.connect()
        self.assertEqual(1, connection.execute("SELECT status FROM channels WHERE id = 26").fetchone()[0])
        self.assertEqual([], connection.execute("SELECT key FROM options").fetchall())
        connection.close()

    def test_slow_channel_tests_use_recent_response_time(self):
        connection = self.connect()
        connection.executemany(
            "INSERT INTO channels(id, name, status, test_time, response_time) VALUES (?, ?, ?, ?, ?)",
            [
                (1, "slow", 1, 950, 16000),
                (2, "fast", 1, 950, 14999),
                (3, "disabled", 3, 950, 60000),
                (4, "old", 1, 800, 60000),
            ],
        )
        connection.commit()

        result = WATCHDOG.load_recent_slow_channel_tests(connection, 900, 15.0)

        self.assertEqual({1: (950, 16000)}, result)
        connection.close()

    def test_hard_latency_is_included_in_channel_test_results(self):
        connection = self.connect()
        connection.execute(
            "INSERT INTO channels(id, name, status, test_time, response_time) VALUES (?, ?, ?, ?, ?)",
            (1, "hard", 1, 950, 45000),
        )
        connection.commit()

        result = WATCHDOG.load_recent_slow_channel_tests(connection, 900, 15.0)

        self.assertEqual({1: (950, 45000)}, result)
        connection.close()

    def test_parse_channel_urls_deduplicates_and_keeps_proxy(self):
        urls, proxy = WATCHDOG.parse_channel_urls(
            "https://one.example",
            '{"base_urls":["https://one.example", "https://two.example"], "proxy":"http://127.0.0.1:7890"}',
        )
        self.assertEqual(["https://one.example", "https://two.example"], urls)
        self.assertEqual("http://127.0.0.1:7890", proxy)


if __name__ == "__main__":
    unittest.main()
