"""Turn the raw run records in results/raw/ into the CSV tables in results/.

    python3 analysis/collect.py            (from the benchmark directory)
"""
import csv, datetime, glob, json, os, re, statistics

HERE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RAW = os.path.join(HERE, "results", "raw")
RES = os.path.join(HERE, "results")


def dur(s):
    """Go duration string ("1h0m11s", "1m4s", "550ms", "0s") to seconds."""
    if not s:
        return None
    total = 0.0
    for num, unit in re.findall(r"([\d.]+)(h|ms|m|s|us|µs|ns)", s):
        total += float(num) * {"h": 3600, "m": 60, "s": 1, "ms": 1e-3, "us": 1e-6, "µs": 1e-6, "ns": 1e-9}[unit]
    return round(total, 1)


def ts(s):
    return datetime.datetime.fromisoformat(s.replace("Z", "+00:00")) if s else None


def secs_between(a, b):
    a, b = ts(a), ts(b)
    return round((b - a).total_seconds(), 1) if a and b else None


def load(suite):
    return [json.load(open(f)) for f in sorted(glob.glob(os.path.join(RAW, suite, "*.json")))]


def write(name, rows):
    if not rows:
        return
    with open(os.path.join(RES, name), "w", newline="") as fh:
        w = csv.DictWriter(fh, fieldnames=list(rows[0].keys()))
        w.writeheader()
        w.writerows(rows)
    print(f"{name}: {len(rows)} rows")


def segment(lsn):
    """WAL segment number (16 MiB segments) that an LSN like "3/65000000" falls in."""
    if not lsn:
        return None
    hi, lo = lsn.split("/")
    return (int(hi, 16) << 32 | int(lo, 16)) >> 24


def leftover_count(l):
    return sum(len(v) for v in (l or {}).values())


# ---------- detection ----------
det = []
for r in load("detection"):
    lr = (r.get("restorecheck") or {}).get("lastRun") or {}
    cl = (r.get("cnpg_cluster_at_verdict") or {}).get("conditions", {})
    backups = r.get("cnpg_backups_at_verdict") or []
    det.append({
        "scenario": r["scenario"], "rep": r["rep"],
        "cnpg_backup_phase": ",".join(sorted({b.get("phase") or "" for b in backups})),
        "cnpg_continuous_archiving": (cl.get("ContinuousArchiving") or {}).get("status"),
        "cnpg_cluster_phase": (r.get("cnpg_cluster_at_verdict") or {}).get("phase"),
        "restorecheck_result": lr.get("result"),
        "restorecheck_reason": lr.get("reason") or "",
        "failed_checks": ";".join(c["name"] for c in lr.get("checks", []) if not c["passed"]),
        "recovery_s": dur(lr.get("recoveryDuration")),
        "data_age_s": dur(lr.get("dataAge")),
        "verdict_s": secs_between(lr.get("startTime"), lr.get("completionTime")),
        "fault_applied": json.dumps(r.get("fault")),
        "leftovers": leftover_count(r.get("leftovers_after_run")),
        "message": (lr.get("message") or "")[:300],
    })
write("detection.csv", det)

# ---------- CronJob baseline ----------
base = []
for r in load("baseline"):
    cj = r.get("cronjob") or {}
    left = r.get("leftovers_after_run") or {}
    # The finished Job and its pod are normal job history, not something the test left running.
    real_left = sum(len([n for n in v if not n.startswith("restore-test-run")]) for v in left.values())
    base.append({
        "scenario": r["scenario"], "rep": r["rep"],
        "cronjob_result": "pass" if cj.get("succeeded") else ("fail" if cj.get("failed") else "unknown"),
        "seconds": secs_between(cj.get("startTime"), cj.get("completionTime")) or secs_between(cj.get("startTime"), next((c["time"] for c in cj.get("conditions", []) if c["type"] == "Failed"), None)),
        "fault_applied": json.dumps(r.get("fault")),
        "leftovers": real_left,
    })
write("baseline.csv", base)

# ---------- size / WAL replay ----------
for suite in ("size", "walreplay"):
    rows = []
    for r in load(suite):
        lr = (r.get("restorecheck") or {}).get("lastRun") or {}
        b = r.get("bucket") or {}
        rows.append({
            "scale": r["scale"], "rep": r["rep"], "load_seconds": r["load_seconds"],
            "db_gb": round(r["db_bytes"] / 1e9, 2),
            "base_backup_gb": round(b.get("base_bytes", 0) / 1e9, 2),
            "wal_gb_after_backup": round(r["wal_bytes_after_backup"] / 1e9, 2),
            "backup_s": secs_between(r.get("backup_started"), r.get("backup_stopped")),
            "restorecheck_result": lr.get("result"),
            "recovery_s": dur(lr.get("recoveryDuration")),
            "recovery_end_segment": segment(lr.get("recoveryEndLSN")),
            "data_age_s": dur(lr.get("dataAge")),
            "verdict_s": secs_between(lr.get("startTime"), lr.get("completionTime")),
            "leftovers": leftover_count(r.get("leftovers_after_run")),
        })
    rows.sort(key=lambda x: (x["scale"], x["load_seconds"], str(x["rep"])))
    write(f"{suite}.csv", rows)

# ---------- reliability ----------
rel = []
for r in load("reliability"):
    lr = r.get("last_run") or {}
    op = r.get("operator") or {}
    mem = op.get("memory", "")
    rel.append({
        "run": r["run_number"], "result": lr.get("result"), "reason": lr.get("reason") or "",
        "recovery_s": dur(lr.get("recoveryDuration")), "recovery_end_segment": segment(lr.get("recoveryEndLSN")),
        "data_age_s": dur(lr.get("dataAge")),
        "verdict_s": secs_between(lr.get("startTime"), lr.get("completionTime")),
        "leftovers": leftover_count(r.get("leftovers_after_run")),
        "operator_memory_mib": int(mem[:-2]) if mem.endswith("Mi") else None,
        "operator_cpu_m": int(op.get("cpu", "0m")[:-1]) if op.get("cpu", "").endswith("m") else None,
    })
rel.sort(key=lambda x: x["run"])
write("reliability.csv", rel)

# ---------- one summary for the report ----------
def med(xs):
    xs = [x for x in xs if x is not None]
    return statistics.median(xs) if xs else None

summary = {
    "detection_runs": len(det),
    "reliability_runs": len(rel),
    "reliability_succeeded": sum(1 for x in rel if x["result"] == "Succeeded"),
    "reliability_leftovers": sum(x["leftovers"] for x in rel),
    "reliability_recovery_median_s": med([x["recovery_s"] for x in rel]),
    "reliability_verdict_median_s": med([x["verdict_s"] for x in rel]),
}
json.dump(summary, open(os.path.join(RES, "summary.json"), "w"), indent=1)
print(json.dumps(summary))
