#!/usr/bin/env python3
"""Conservative new-api channel isolation watchdog.

The native new-api auto-disable/auto-recovery controls remain disabled so they
cannot bypass the all-Base-URL failure requirement. This helper isolates an
enabled channel for authentication or balance failures, or after three
transient failures in five minutes *and* every configured Base URL is
unreachable. It never automatically enables a channel.
"""

from __future__ import annotations

import argparse
import json
import logging
from logging.handlers import RotatingFileHandler
import math
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import sys
import time
from datetime import datetime, timezone
from typing import Any
from urllib.error import HTTPError, URLError
from urllib.request import ProxyHandler, Request, build_opener

try:
    import fcntl
except ImportError:  # pragma: no cover - the production service runs on Linux.
    fcntl = None


DEFAULTS = {
    "NEW_API_CHANNEL_WATCHDOG_DB": "/opt/new-api/data/one-api.db",
    "NEW_API_CHANNEL_WATCHDOG_STATE": "/var/lib/new-api-channel-watchdog/state.json",
    "NEW_API_CHANNEL_WATCHDOG_LOG": "/var/log/new-api-channel-watchdog/events.log",
    "NEW_API_CHANNEL_WATCHDOG_BACKUP_DIR": "/opt/ops-backups/new-api",
    "NEW_API_CHANNEL_WATCHDOG_ALERT_COMMAND": "/usr/local/sbin/sub2api-alert",
    "NEW_API_CHANNEL_WATCHDOG_WINDOW_SECONDS": "300",
    "NEW_API_CHANNEL_WATCHDOG_TRANSIENT_FAILURES": "3",
    "NEW_API_CHANNEL_WATCHDOG_PROBE_TIMEOUT_SECONDS": "10",
    "NEW_API_CHANNEL_WATCHDOG_SLOW_SECONDS": "15",
    "NEW_API_CHANNEL_WATCHDOG_HARD_SECONDS": "45",
    "NEW_API_CHANNEL_WATCHDOG_TEST_RESULT_MAX_AGE_SECONDS": "1200",
    "NEW_API_CHANNEL_WATCHDOG_COMPOSE_FILE": "/opt/new-api/docker-compose.yml",
    "NEW_API_CHANNEL_WATCHDOG_HEALTH_URL": "http://127.0.0.1:3000/api/status",
    "NEW_API_CHANNEL_WATCHDOG_RESTART_TIMEOUT_SECONDS": "90",
}
CONFIG_PATH = Path("/etc/new-api-channel-watchdog.env")

AUTH_OR_BALANCE_PATTERNS = (
    "insufficient_balance",
    "insufficient_user_quota",
    "insufficient balance",
    "insufficient account balance",
    "credit balance is too low",
    "your credit balance is too low",
    "quota has been exhausted",
    "allocated quota exceeded",
    "this organization has been disabled",
    "permission denied",
    "not authorized",
    "not authorized to",
    "security token included in the request is invalid",
    "status_code=401",
    "status_code=403",
    "status code: 401",
    "status code: 403",
    "余额不足",
    "额度不足",
    "余额已用完",
)
CANCELLED_PATTERNS = (
    "context canceled",
    "context cancelled",
    "client canceled",
    "client cancelled",
    "client_gone",
    "broken pipe",
)
MODEL_OR_ROUTE_FAILURE_PATTERNS = (
    "model_not_found",
    "model not found",
    "model not supported",
    "unsupported model",
    "not implemented",
    "unsupported endpoint",
    "endpoint not supported",
    "no available channel for model",
    "no available channel for this model",
    "route not found",
)
TRANSIENT_FAILURE_RE = re.compile(
    r"(?:status[_ ]code[=: ]+5\d\d|error code:? 524|gateway time-?out|"
    r"connection reset|connection refused|deadline exceeded|timeout|"
    r"unexpected eof|stream_read_error|transport)",
    re.IGNORECASE,
)


def load_config() -> dict[str, str]:
    config = dict(DEFAULTS)
    if CONFIG_PATH.is_file():
        for raw_line in CONFIG_PATH.read_text(encoding="utf-8").splitlines():
            line = raw_line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, value = line.split("=", 1)
            key = key.strip()
            if key in config:
                config[key] = value.strip()
    for key in config:
        if key in os.environ:
            config[key] = os.environ[key]
    return config


def positive_int(config: dict[str, str], key: str) -> int:
    try:
        value = int(config[key])
    except (KeyError, ValueError) as exc:
        raise ValueError(f"{key} must be a positive integer") from exc
    if value <= 0:
        raise ValueError(f"{key} must be a positive integer")
    return value


def positive_float(config: dict[str, str], key: str) -> float:
    try:
        value = float(config[key])
    except (KeyError, ValueError) as exc:
        raise ValueError(f"{key} must be a positive number") from exc
    if not math.isfinite(value) or value <= 0:
        raise ValueError(f"{key} must be a positive number")
    return value


def format_number(value: float) -> str:
    return f"{value:g}"


def build_logger(path: Path) -> logging.Logger:
    path.parent.mkdir(parents=True, exist_ok=True)
    logger = logging.getLogger("new-api-channel-watchdog")
    logger.setLevel(logging.INFO)
    logger.handlers.clear()
    handler = RotatingFileHandler(path, maxBytes=1_048_576, backupCount=5, encoding="utf-8")
    formatter = logging.Formatter("%(asctime)sZ %(levelname)s %(message)s")
    formatter.converter = time.gmtime
    handler.setFormatter(formatter)
    logger.addHandler(handler)
    return logger


def notify(config: dict[str, str], severity: str, event: str, message: str) -> None:
    command = Path(config["NEW_API_CHANNEL_WATCHDOG_ALERT_COMMAND"])
    if not command.is_file() or not os.access(command, os.X_OK):
        return
    try:
        subprocess.run(
            [str(command), severity, event, message],
            check=False,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            timeout=20,
        )
    except (OSError, subprocess.TimeoutExpired):
        return


def emit_event(
    config: dict[str, str],
    logger: logging.Logger,
    dry_run: bool,
    severity: str,
    event: str,
    message: str,
) -> None:
    if dry_run:
        logger.info("dry run: %s", message)
        return
    notify(config, severity, event, message)
    if severity == "info":
        logger.info(message)
    elif severity == "warning":
        logger.warning(message)
    else:
        logger.error(message)


def read_json(path: Path) -> dict[str, Any]:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (FileNotFoundError, json.JSONDecodeError):
        return {}


def write_json(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, ensure_ascii=True, sort_keys=True) + "\n", encoding="utf-8")
    os.chmod(temporary, 0o600)
    temporary.replace(path)


def backup_database(db_path: Path, backup_dir: Path, label: str) -> Path:
    backup_dir.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    target = backup_dir / f"one-api-{label}-{stamp}.db"
    source = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True, timeout=10)
    destination = sqlite3.connect(target, timeout=10)
    try:
        source.backup(destination)
    finally:
        destination.close()
        source.close()
    os.chmod(target, 0o600)
    return target


def backup_channel_state(
    state_path: Path,
    backup_dir: Path,
    channels: list[sqlite3.Row],
    state: dict[str, Any],
    label: str,
) -> Path:
    """Save a credential-free state snapshot before mutating channel data."""
    backup_dir.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    target = backup_dir / f"channel-state-{label}-{stamp}.json"
    suffix = 1
    while target.exists():
        target = backup_dir / f"channel-state-{label}-{stamp}-{suffix}.json"
        suffix += 1

    payload = {
        "captured_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
        "source": str(state_path),
        "statuses": {
            str(int(channel["id"])): int(channel["status"])
            for channel in channels
        },
        "watchdog_state": state,
    }
    temporary = target.with_suffix(target.suffix + ".tmp")
    temporary.write_text(
        json.dumps(payload, ensure_ascii=True, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    os.chmod(temporary, 0o600)
    temporary.replace(target)
    return target


def parse_channel_urls(base_url: str | None, setting: str | None) -> tuple[list[str], str]:
    urls: list[str] = []
    proxy = ""
    if base_url and base_url.strip():
        urls.append(base_url.strip())
    if setting:
        try:
            decoded = json.loads(setting)
        except json.JSONDecodeError:
            decoded = {}
        if isinstance(decoded, dict):
            candidate_proxy = decoded.get("proxy")
            if isinstance(candidate_proxy, str):
                proxy = candidate_proxy.strip()
            candidates = decoded.get("base_urls")
            if isinstance(candidates, list):
                for candidate in candidates:
                    if isinstance(candidate, str) and candidate.strip():
                        urls.append(candidate.strip())
    unique_urls: list[str] = []
    seen: set[str] = set()
    for url in urls:
        if url not in seen:
            seen.add(url)
            unique_urls.append(url)
    return unique_urls, proxy


def all_base_urls_unreachable(urls: list[str], proxy: str, timeout: int) -> bool:
    if not urls:
        return False
    handlers = []
    if proxy:
        handlers.append(ProxyHandler({"http": proxy, "https": proxy}))
    else:
        handlers.append(ProxyHandler({}))
    opener = build_opener(*handlers)
    for url in urls:
        try:
            request = Request(url.rstrip("/"), method="GET", headers={"User-Agent": "new-api-channel-watchdog/1.0"})
            response = opener.open(request, timeout=timeout)
            status = getattr(response, "status", response.getcode())
            response.close()
            if status < 500:
                return False
        except HTTPError as exc:
            if exc.code < 500:
                return False
        except (URLError, OSError, ValueError):
            continue
    return True


def error_is_cancelled(text: str) -> bool:
    lowered = text.lower()
    return any(pattern in lowered for pattern in CANCELLED_PATTERNS)


def error_is_model_or_route_failure(text: str) -> bool:
    lowered = text.lower()
    return any(pattern in lowered for pattern in MODEL_OR_ROUTE_FAILURE_PATTERNS)


def error_is_auth_or_balance(text: str) -> bool:
    if error_is_cancelled(text) or error_is_model_or_route_failure(text):
        return False
    lowered = text.lower()
    return any(pattern in lowered for pattern in AUTH_OR_BALANCE_PATTERNS)


def error_is_transient(text: str) -> bool:
    return (
        not error_is_cancelled(text)
        and not error_is_model_or_route_failure(text)
        and TRANSIENT_FAILURE_RE.search(text) is not None
    )


def load_channels(connection: sqlite3.Connection) -> list[sqlite3.Row]:
    return connection.execute(
        "SELECT id, name, status, base_url, setting FROM channels ORDER BY id"
    ).fetchall()


def load_recent_errors(connection: sqlite3.Connection, window_start: int) -> dict[int, list[str]]:
    rows = connection.execute(
        """
        SELECT channel_id, COALESCE(content, ''), COALESCE(other, '')
        FROM logs
        WHERE type = 5 AND channel_id IS NOT NULL AND created_at >= ?
        ORDER BY id ASC
        """,
        (window_start,),
    ).fetchall()
    errors: dict[int, list[str]] = {}
    for channel_id, content, other in rows:
        errors.setdefault(int(channel_id), []).append(f"{content}\n{other}")
    return errors


def load_recent_slow_channel_tests(
    connection: sqlite3.Connection,
    observed_since: int,
    slow_seconds: float,
) -> dict[int, tuple[int, int]]:
    rows = connection.execute(
        """
        SELECT id, test_time, response_time
        FROM channels
        WHERE status = 1
          AND test_time >= ?
          AND response_time >= ?
        """,
        (observed_since, int(slow_seconds * 1000)),
    ).fetchall()
    return {
        int(channel_id): (int(test_time), int(response_time))
        for channel_id, test_time, response_time in rows
    }


def isolate_channels(
    db_path: Path,
    channel_ids: list[int],
    dry_run: bool,
) -> list[int]:
    if not channel_ids:
        return []
    if dry_run:
        return channel_ids
    connection = sqlite3.connect(db_path, timeout=15)
    try:
        connection.execute("PRAGMA busy_timeout = 15000")
        connection.execute("BEGIN IMMEDIATE")
        changed: list[int] = []
        for channel_id in channel_ids:
            result = connection.execute(
                "UPDATE channels SET status = 3 WHERE id = ? AND status = 1",
                (channel_id,),
            )
            if result.rowcount == 1:
                connection.execute(
                    "UPDATE abilities SET enabled = 0 WHERE channel_id = ?",
                    (channel_id,),
                )
                changed.append(channel_id)
        connection.commit()
        return changed
    except Exception:
        connection.rollback()
        raise
    finally:
        connection.close()


def set_options_and_initial_isolation(
    db_path: Path,
    requested_channel_ids: list[int],
    dry_run: bool,
    hard_seconds: float,
) -> tuple[list[int], list[str]]:
    connection = sqlite3.connect(db_path, timeout=15)
    connection.row_factory = sqlite3.Row
    try:
        connection.execute("PRAGMA busy_timeout = 15000")
        current_keywords_row = connection.execute(
            "SELECT value FROM options WHERE key = 'AutomaticDisableKeywords'"
        ).fetchone()
        current_keywords = current_keywords_row["value"] if current_keywords_row else ""
        required_keywords = [
            "Insufficient account balance",
            "insufficient_balance",
            "insufficient balance",
            "Your credit balance is too low",
            "quota has been exhausted",
            "allocated quota exceeded",
        ]
        keyword_lines: list[str] = []
        seen: set[str] = set()
        for raw in current_keywords.splitlines() + required_keywords:
            item = raw.strip()
            if item and item.lower() not in seen:
                seen.add(item.lower())
                keyword_lines.append(item)

        values = {
            # The watchdog owns isolation so native new-api behavior cannot
            # disable a channel before all configured Base URLs are checked.
            "AutomaticDisableChannelEnabled": "false",
            "AutomaticEnableChannelEnabled": "false",
            "AutomaticDisableStatusCodes": "401,403",
            "AutomaticDisableKeywords": "\n".join(keyword_lines),
            "ChannelDisableThreshold": format_number(hard_seconds),
            "monitor_setting.auto_test_channel_enabled": "true",
            "monitor_setting.auto_test_channel_minutes": "10",
            "monitor_setting.channel_test_mode": "scheduled_all",
        }
        changed_options = sorted(values)
        if dry_run:
            active = {
                int(row["id"])
                for row in connection.execute(
                    "SELECT id FROM channels WHERE status = 1 AND id IN ({})".format(
                        ",".join("?" for _ in requested_channel_ids) or "NULL"
                    ),
                    requested_channel_ids,
                )
            }
            return sorted(active), changed_options

        connection.execute("BEGIN IMMEDIATE")
        for key, value in values.items():
            connection.execute(
                "INSERT INTO options(key, value) VALUES(?, ?) "
                "ON CONFLICT(key) DO UPDATE SET value = excluded.value",
                (key, value),
            )
        changed_channels: list[int] = []
        for channel_id in requested_channel_ids:
            result = connection.execute(
                "UPDATE channels SET status = 3 WHERE id = ? AND status = 1",
                (channel_id,),
            )
            if result.rowcount == 1:
                connection.execute(
                    "UPDATE abilities SET enabled = 0 WHERE channel_id = ?",
                    (channel_id,),
                )
                changed_channels.append(channel_id)
        connection.commit()
        return changed_channels, changed_options
    except Exception:
        connection.rollback()
        raise
    finally:
        connection.close()


def restart_new_api(config: dict[str, str], logger: logging.Logger) -> bool:
    compose_file = Path(config["NEW_API_CHANNEL_WATCHDOG_COMPOSE_FILE"])
    health_url = config["NEW_API_CHANNEL_WATCHDOG_HEALTH_URL"].strip()
    try:
        restart = subprocess.run(
            ["docker", "compose", "-f", str(compose_file), "restart", "new-api"],
            check=False,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
            text=True,
            timeout=30,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        logger.error("new-api restart failed: %s", exc)
        return False
    if restart.returncode != 0:
        detail = (restart.stderr or "").strip().replace("\n", " ")[-300:]
        logger.error("new-api restart failed: %s", detail or "unknown error")
        return False

    try:
        timeout = positive_int(config, "NEW_API_CHANNEL_WATCHDOG_RESTART_TIMEOUT_SECONDS")
    except ValueError as exc:
        logger.error("invalid restart health timeout: %s", exc)
        return False
    deadline = time.monotonic() + timeout
    opener = build_opener(ProxyHandler({}))
    while time.monotonic() < deadline:
        try:
            request = Request(health_url, method="GET", headers={"User-Agent": "new-api-channel-watchdog/1.0"})
            with opener.open(request, timeout=5) as response:
                status = getattr(response, "status", response.getcode())
                if 200 <= status < 400:
                    logger.info("new-api restarted and health check passed: status=%s", status)
                    return True
        except (HTTPError, URLError, OSError, ValueError):
            pass
        time.sleep(1)
    logger.error("new-api restart health check timed out: %s", health_url)
    return False


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dry-run", action="store_true", help="report candidate actions without updating SQLite")
    parser.add_argument("--configure-governance", action="store_true", help="apply the approved new-api safety defaults")
    parser.add_argument("--isolate-channel", type=int, action="append", default=[], help="explicitly auto-disable one enabled channel")
    parser.add_argument("--reason", default="channel governance watchdog", help="audit text for explicit isolation")
    parser.add_argument("--restart-new-api", action="store_true", help="restart new-api after a database mutation and verify its health")
    args = parser.parse_args()

    config = load_config()
    try:
        window = positive_int(config, "NEW_API_CHANNEL_WATCHDOG_WINDOW_SECONDS")
        transient_threshold = positive_int(config, "NEW_API_CHANNEL_WATCHDOG_TRANSIENT_FAILURES")
        probe_timeout = positive_int(config, "NEW_API_CHANNEL_WATCHDOG_PROBE_TIMEOUT_SECONDS")
        slow_seconds = positive_float(config, "NEW_API_CHANNEL_WATCHDOG_SLOW_SECONDS")
        hard_seconds = positive_float(config, "NEW_API_CHANNEL_WATCHDOG_HARD_SECONDS")
        test_result_max_age = positive_int(config, "NEW_API_CHANNEL_WATCHDOG_TEST_RESULT_MAX_AGE_SECONDS")
    except ValueError as exc:
        print(str(exc), file=sys.stderr)
        return 2
    if slow_seconds >= hard_seconds:
        print("NEW_API_CHANNEL_WATCHDOG_SLOW_SECONDS must be lower than hard threshold", file=sys.stderr)
        return 2

    db_path = Path(config["NEW_API_CHANNEL_WATCHDOG_DB"])
    state_path = Path(config["NEW_API_CHANNEL_WATCHDOG_STATE"])
    backup_dir = Path(config["NEW_API_CHANNEL_WATCHDOG_BACKUP_DIR"])
    if not db_path.is_file():
        print(f"database does not exist: {db_path}", file=sys.stderr)
        return 1

    logger = build_logger(Path(config["NEW_API_CHANNEL_WATCHDOG_LOG"]))
    state_path.parent.mkdir(parents=True, exist_ok=True)
    if fcntl is None:
        logger.error("this watchdog requires a POSIX host with fcntl file locks")
        return 2
    lock_path = state_path.with_suffix(".lock")
    with lock_path.open("a+") as lock_file:
        try:
            fcntl.flock(lock_file.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            logger.info("another watchdog process is already running")
            return 0

        now = int(time.time())
        read_connection = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True, timeout=10)
        read_connection.row_factory = sqlite3.Row
        try:
            channels = load_channels(read_connection)
            recent_errors = load_recent_errors(read_connection, now - window)
            slow_channel_tests = load_recent_slow_channel_tests(
                read_connection,
                now - test_result_max_age,
                slow_seconds,
            )
        finally:
            read_connection.close()

        channels_by_id = {int(channel["id"]): channel for channel in channels}
        state = read_json(state_path)
        previous_alerted_tests = {
            int(key): int(value)
            for key, value in state.get(
                "channel_test_times",
                state.get("slow_test_times", {}),
            ).items()
            if str(key).isdigit() and isinstance(value, int)
        }
        alerted_test_times: dict[int, int] = {}
        slow_channel_ids: list[int] = []
        hard_channel_ids: list[int] = []
        for channel_id, (test_time, response_time_ms) in slow_channel_tests.items():
            alerted_test_times[channel_id] = test_time
            if test_time <= previous_alerted_tests.get(channel_id, 0):
                continue
            channel = channels_by_id.get(channel_id)
            if channel is None:
                continue
            response_seconds = response_time_ms / 1000.0
            if response_seconds >= hard_seconds:
                message = (
                    f"channel #{channel_id} {channel['name']} hard health-test failure: "
                    f"{response_seconds:.2f}s (hard limit {hard_seconds:g}s); "
                    "not isolated until the transient failure rule is met"
                )
                emit_event(
                    config,
                    logger,
                    args.dry_run,
                    "error",
                    "new_api_channel_hard_latency",
                    message,
                )
                hard_channel_ids.append(channel_id)
            else:
                message = (
                    f"channel #{channel_id} {channel['name']} slow health test: "
                    f"{response_seconds:.2f}s (warning at {slow_seconds:g}s)"
                )
                emit_event(config, logger, args.dry_run, "warning", "new_api_channel_slow", message)
                slow_channel_ids.append(channel_id)

        candidates: dict[int, str] = {}
        for channel in channels:
            channel_id = int(channel["id"])
            if int(channel["status"]) != 1:
                continue
            errors = recent_errors.get(channel_id, [])
            if any(error_is_auth_or_balance(error) for error in errors):
                candidates[channel_id] = "authentication_or_balance_failure"
                continue
            transient_count = sum(1 for error in errors if error_is_transient(error))
            if transient_count < transient_threshold:
                continue
            urls, proxy = parse_channel_urls(channel["base_url"], channel["setting"])
            if all_base_urls_unreachable(urls, proxy, probe_timeout):
                candidates[channel_id] = f"{transient_count}_transient_failures_all_base_urls_unreachable"

        explicit_ids = sorted({channel_id for channel_id in args.isolate_channel if channel_id > 0})
        for channel_id in explicit_ids:
            channel = channels_by_id.get(channel_id)
            if channel is not None and int(channel["status"]) == 1:
                candidates[channel_id] = args.reason.strip() or "explicit_isolation"

        changes_requested = bool(candidates) or args.configure_governance
        if changes_requested and not args.dry_run:
            backup = backup_database(db_path, backup_dir, "before-channel-governance")
            logger.info("created SQLite governance backup: %s", backup.name)
            state_backup = backup_channel_state(
                state_path,
                backup_dir,
                channels,
                state,
                "before-channel-governance",
            )
            logger.info("created channel state backup: %s", state_backup.name)

        configured_options: list[str] = []
        configured_ids: list[int] = []
        if args.configure_governance:
            configured_ids, configured_options = set_options_and_initial_isolation(
                db_path,
                explicit_ids,
                args.dry_run,
                hard_seconds,
            )
            for channel_id in configured_ids:
                candidates.pop(channel_id, None)

        isolated_ids = isolate_channels(db_path, sorted(candidates), args.dry_run)
        isolated_reasons = {channel_id: candidates[channel_id] for channel_id in isolated_ids}
        if args.configure_governance:
            for channel_id in configured_ids:
                isolated_reasons[channel_id] = args.reason.strip() or "initial_confirmed_incident"

        previous_statuses = {
            int(key): int(value)
            for key, value in state.get("statuses", {}).items()
            if str(key).isdigit() and isinstance(value, int)
        }
        current_statuses = {int(channel["id"]): int(channel["status"]) for channel in channels}
        for channel_id in isolated_reasons:
            current_statuses[channel_id] = 3

        if state.get("initialized"):
            for channel_id, status in current_statuses.items():
                previous = previous_statuses.get(channel_id)
                if previous == 3 and status == 1 and channel_id not in isolated_reasons:
                    channel_name = str(channels_by_id[channel_id]["name"])
                    emit_event(
                        config,
                        logger,
                        args.dry_run,
                        "info",
                        "new_api_channel_manual_recovery",
                        f"channel #{channel_id} {channel_name} was re-enabled",
                    )
                elif previous is not None and previous != 3 and status == 3 and channel_id not in isolated_reasons:
                    channel_name = str(channels_by_id[channel_id]["name"])
                    emit_event(
                        config,
                        logger,
                        args.dry_run,
                        "warning",
                        "new_api_channel_disabled_externally",
                        f"channel #{channel_id} {channel_name} was disabled outside the watchdog; manual verification required",
                    )

        for channel_id, reason in isolated_reasons.items():
            channel_name = str(channels_by_id[channel_id]["name"])
            message = f"channel #{channel_id} {channel_name} isolated: {reason}"
            severity = "warning" if "authentication" in reason or "balance" in reason else "error"
            emit_event(config, logger, args.dry_run, severity, "new_api_channel_isolated", message)

        if args.configure_governance:
            message = "new-api channel governance configured: " + ", ".join(configured_options)
            emit_event(config, logger, args.dry_run, "info", "new_api_channel_governance_configured", message)

        if not args.dry_run:
            write_json(
                state_path,
                {
                    "initialized": True,
                    "updated_at": datetime.now(timezone.utc).isoformat().replace("+00:00", "Z"),
                    "statuses": {str(key): value for key, value in sorted(current_statuses.items())},
                    "channel_test_times": {str(key): value for key, value in sorted(alerted_test_times.items())},
                },
            )

        restarted_new_api = False
        if args.restart_new_api and not args.dry_run and (args.configure_governance or bool(isolated_ids)):
            restarted_new_api = restart_new_api(config, logger)
            if not restarted_new_api:
                notify(config, "critical", "new_api_restart_failed", "new-api database governance changed but restart health verification failed")
                return 1

        print(
            json.dumps(
                {
                    "configured": bool(args.configure_governance),
                    "configured_options": configured_options,
                    "dry_run": bool(args.dry_run),
                    "isolated_channel_ids": sorted(isolated_reasons),
                    "hard_channel_ids": sorted(hard_channel_ids),
                    "slow_channel_ids": sorted(slow_channel_ids),
                    "slow_seconds": slow_seconds,
                    "hard_seconds": hard_seconds,
                    "restarted_new_api": restarted_new_api,
                    "transient_window_seconds": window,
                },
                ensure_ascii=True,
                sort_keys=True,
            )
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
