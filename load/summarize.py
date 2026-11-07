#!/usr/bin/env python3
"""Consolidate replication load-test results.

Reads the k6 summary JSON files produced by the scripts in this directory and
writes summary.csv / summary.md / summary.json next to them (not into git).

    ./summarize.py                  # load/results
    ./summarize.py --results DIR
"""
from __future__ import annotations

import argparse
import csv
import json
import re
import statistics
import sys
from pathlib import Path

ENDPOINTS = ("get", "search")
RUN_RE = re.compile(r"^vu_(\d+)_run_(\d+)\.json$")
DURATION_RE = re.compile(r"^(\d+(?:\.\d+)?)(ms|s|m|h)$")


def parse_duration(text: str) -> float:
    m = DURATION_RE.match(str(text).strip())
    if not m:
        raise ValueError(f"unsupported duration: {text!r}")
    value = float(m.group(1))
    unit = m.group(2)
    return value * {"ms": 0.001, "s": 1, "m": 60, "h": 3600}[unit]


def load_json(path: Path):
    with path.open(encoding="utf-8") as f:
        return json.load(f)


def metric(data: dict, key: str) -> dict:
    return data.get("metrics", {}).get(key, {}).get("values", {})


def window_seconds(data: dict, ramp: float = 0.0) -> tuple[float, float]:
    """Return (whole run, steady-state) duration from the k6 state block.

    k6 does not export the scenario definition in its summary, so the ramp of
    the 1000-VU level is passed in by the caller.
    """
    total = float(data.get("state", {}).get("testRunDurationMs", 0)) / 1000.0
    if total <= 0:
        total = 1.0
    return total, max(total - ramp, 0.0)


def endpoint_metric(data: dict, name: str, endpoint: str) -> dict:
    sub = f"{name}{{endpoint:{endpoint}}}"
    return metric(data, sub) or metric(data, name)


def read_run(data: dict, phase: str, vus: int, run: int, ramp_1000: float) -> list[dict]:
    ramp = ramp_1000 if vus >= 1000 else 0.0
    total, steady = window_seconds(data, ramp)
    steady = steady or total
    rows = []
    for endpoint in ENDPOINTS:
        counter = "get_requests" if endpoint == "get" else "search_requests"
        count = int(metric(data, counter).get("count", 0))
        dur = endpoint_metric(data, "http_req_duration", endpoint)
        failed = endpoint_metric(data, "http_req_failed", endpoint)

        def ms(key: str) -> float | None:
            value = dur.get(key)
            return None if value is None else float(value)

        rows.append(
            {
                "phase": phase,
                "vus": vus,
                "run": run,
                "endpoint": endpoint,
                "count": count,
                "rps": count / total if total else 0.0,
                "rps_steady": count / steady if steady else 0.0,
                "avg_ms": ms("avg"),
                "p50_ms": ms("p(50)"),
                "p95_ms": ms("p(95)"),
                "p99_ms": ms("p(99)"),
                "failed_rate": float(failed.get("rate", 0.0)),
            }
        )
    return rows


def median(values):
    values = [v for v in values if v is not None]
    return statistics.median(values) if values else None


def collect_reads(results: Path, ramp_1000: float) -> list[dict]:
    rows = []
    for phase in ("baseline", "replicas"):
        phase_dir = results / phase
        if not phase_dir.is_dir():
            continue
        for path in sorted(phase_dir.glob("vu_*_run_*.json")):
            m = RUN_RE.match(path.name)
            if not m:
                continue
            rows.extend(read_run(load_json(path), phase, int(m.group(1)), int(m.group(2)), ramp_1000))
    return rows


def aggregate(rows: list[dict]) -> list[dict]:
    groups: dict[tuple, list[dict]] = {}
    for row in rows:
        groups.setdefault((row["phase"], row["vus"], row["endpoint"]), []).append(row)

    out = []
    for (phase, vus, endpoint), items in sorted(groups.items(), key=lambda kv: (kv[0][0], kv[0][1], kv[0][2])):
        out.append(
            {
                "phase": phase,
                "vus": vus,
                "endpoint": endpoint,
                "runs": len(items),
                "count": median([i["count"] for i in items]),
                "rps": median([i["rps"] for i in items]),
                "rps_steady": median([i["rps_steady"] for i in items]),
                "avg_ms": median([i["avg_ms"] for i in items]),
                "p50_ms": median([i["p50_ms"] for i in items]),
                "p95_ms": median([i["p95_ms"] for i in items]),
                "p99_ms": median([i["p99_ms"] for i in items]),
                "failed_rate": median([i["failed_rate"] for i in items]),
            }
        )
    return out


def collect_write(results: Path, mode: str) -> dict | None:
    path = results / f"write-{mode}" / "summary.json"
    if not path.is_file():
        return None
    data = load_json(path)
    ok = int(metric(data, "writes_ok").get("count", 0))
    failed = int(metric(data, "writes_failed").get("count", 0))
    dur = metric(data, "http_req_duration")
    total, _ = window_seconds(data)
    return {
        "mode": mode,
        "acked": ok,
        "failed": failed,
        "rps": metric(data, "http_reqs").get("count", 0) / total if total else 0.0,
        "p50_ms": dur.get("p(50)"),
        "p95_ms": dur.get("p(95)"),
        "p99_ms": dur.get("p(99)"),
    }


def collect_failover(results: Path, mode: str) -> dict | None:
    path = results / f"failover-{mode}" / "summary.json"
    if not path.is_file():
        return None
    return load_json(path)


def collect_routing(results: Path, phase: str) -> dict | None:
    path = results / phase / "replication_after.json"
    if not path.is_file():
        return None
    return load_json(path).get("routing")


def fmt(value, digits=1, suffix=""):
    if value is None:
        return "—"
    return f"{value:.{digits}f}{suffix}"


def build_markdown(agg, writes, failovers, routing) -> str:
    lines = ["# Сводка нагрузочных прогонов", ""]

    lines += ["## Чтение: медианы по прогонам", ""]
    lines += [
        "| Фаза | VU | Эндпоинт | Запросов | RPS (steady) | p50, мс | p95, мс | p99, мс | Ошибки |",
        "|---|---:|---|---:|---:|---:|---:|---:|---:|",
    ]
    for row in agg:
        lines.append(
            f"| {row['phase']} | {row['vus']} | {row['endpoint']} | "
            f"{int(row['count']) if row['count'] is not None else '—'} | "
            f"{fmt(row['rps_steady'])} | {fmt(row['p50_ms'])} | {fmt(row['p95_ms'])} | "
            f"{fmt(row['p99_ms'])} | {fmt((row['failed_rate'] or 0) * 100, 2, '%')} |"
        )

    by_key = {(r["phase"], r["vus"], r["endpoint"]): r for r in agg}
    pairs = [k for k in by_key if k[0] == "baseline"]
    if pairs:
        lines += ["", "## Эффект чтения с реплик", "", "| VU | Эндпоинт | RPS base | RPS repl | Прирост | p95 base, мс | p95 repl, мс |", "|---:|---|---:|---:|---:|---:|---:|"]
        for _, vus, endpoint in sorted(pairs, key=lambda k: (k[1], k[2])):
            base = by_key.get(("baseline", vus, endpoint))
            repl = by_key.get(("replicas", vus, endpoint))
            if not base or not repl:
                continue
            gain = (repl["rps_steady"] / base["rps_steady"]) if base["rps_steady"] else None
            lines.append(
                f"| {vus} | {endpoint} | {fmt(base['rps_steady'])} | {fmt(repl['rps_steady'])} | "
                f"{fmt(gain, 2, '×')} | {fmt(base['p95_ms'])} | {fmt(repl['p95_ms'])} |"
            )

    if routing:
        lines += ["", "## Маршрутизация чтений", ""]
        for phase, stats in routing.items():
            if not stats:
                continue
            lines.append(
                f"- {phase}: реплики {stats.get('replica_reads', 0)}, "
                f"master {stats.get('master_reads', 0)}, fallback {stats.get('master_fallbacks', 0)}"
            )

    if any(writes.values()):
        lines += ["", "## Запись: async vs sync", ""]
        lines += ["| Режим | Подтверждено | Ошибок | RPS | p50, мс | p95, мс | p99, мс |", "|---|---:|---:|---:|---:|---:|---:|"]
        for mode in ("async", "sync"):
            w = writes.get(mode)
            if w:
                lines.append(
                    f"| {w['mode']} | {w['acked']} | {w['failed']} | {fmt(w['rps'])} | "
                    f"{fmt(w['p50_ms'])} | {fmt(w['p95_ms'])} | {fmt(w['p99_ms'])} |"
                )

    if any(failovers.values()):
        lines += ["", "## Failover", ""]
        lines += ["| Режим | Промоут | Подтверждено | Ошибок | Строк | Потеряно | Неопределённых |", "|---|---|---:|---:|---:|---:|---:|"]
        for mode in ("async", "sync"):
            f = failovers.get(mode)
            if f:
                lines.append(
                    f"| {f['mode']} | {f['promoted']} | {f['acked_writes']} | {f['failed_writes']} | "
                    f"{f['rows_after_failover']} | {f['lost_acked_writes']} | {f['unconfirmed_rows']} |"
                )

    return "\n".join(lines) + "\n"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--results", type=Path, default=Path(__file__).resolve().parent / "results")
    parser.add_argument("--ramp-1000", type=float, default=5.0,
                        help="ramp-up seconds of the 1000-VU level (default 5)")
    args = parser.parse_args()

    results: Path = args.results
    if not results.is_dir():
        print(f"no results directory: {results}", file=sys.stderr)
        return 1

    rows = collect_reads(results, args.ramp_1000)
    agg = aggregate(rows)

    writes = {mode: collect_write(results, mode) for mode in ("async", "sync")}
    failovers = {mode: collect_failover(results, mode) for mode in ("async", "sync")}
    routing = {phase: collect_routing(results, phase) for phase in ("baseline", "replicas")}

    with (results / "summary.csv").open("w", newline="", encoding="utf-8") as f:
        writer = csv.DictWriter(f, fieldnames=list(rows[0].keys()) if rows else ["phase"])
        writer.writeheader()
        writer.writerows(rows)

    summary = {
        "runs": rows,
        "aggregated": agg,
        "writes": {k: v for k, v in writes.items() if v},
        "failovers": {k: v for k, v in failovers.items() if v},
        "routing": {k: v for k, v in routing.items() if v},
    }
    (results / "summary.json").write_text(
        json.dumps(summary, indent=2, ensure_ascii=False) + "\n", encoding="utf-8"
    )
    (results / "summary.md").write_text(
        build_markdown(agg, writes, failovers, routing), encoding="utf-8"
    )

    print((results / "summary.md").read_text(encoding="utf-8"))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
