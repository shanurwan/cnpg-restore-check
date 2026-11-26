"""Draw the charts in charts/ from the CSV tables in results/.

    python3 analysis/plot.py            (needs matplotlib)
"""
import csv, os, statistics
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

HERE = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
RES = os.path.join(HERE, "results")
OUT = os.path.join(HERE, "charts")
os.makedirs(OUT, exist_ok=True)

INK, MUTED, GRID = "#1c2526", "#5b6b6e", "#d9e1e3"
OK, BAD, WARN, ACCENT = "#2e7d4f", "#c0392b", "#b7791f", "#0e6e66"
plt.rcParams.update({
    "font.family": "DejaVu Sans", "font.size": 10, "axes.edgecolor": MUTED, "axes.labelcolor": INK,
    "xtick.color": MUTED, "ytick.color": MUTED, "axes.titleweight": "bold", "axes.titlesize": 12,
    "axes.spines.top": False, "axes.spines.right": False, "figure.dpi": 150, "savefig.bbox": "tight",
})

ORDER = ["healthy", "wal-gap", "backup-wal-missing", "base-data-deleted", "heap-corruption", "logical-loss", "archiving-stopped"]
LABEL = {
    "healthy": "Healthy (control)", "wal-gap": "WAL segment missing after backup",
    "backup-wal-missing": "WAL missing inside base backup", "base-data-deleted": "Base backup data deleted",
    "heap-corruption": "Heap page corrupted in backup", "logical-loss": "Half the rows deleted by the app",
    "archiving-stopped": "WAL archiving stopped",
}


def rows(name):
    p = os.path.join(RES, name)
    return list(csv.DictReader(open(p))) if os.path.exists(p) else []


def f(x):
    return float(x) if x not in (None, "", "None") else None


def save(fig, name):
    fig.savefig(os.path.join(OUT, name), facecolor="white")
    plt.close(fig)
    print("charts/" + name)


# 1. Detection matrix: what each approach reported about the same broken backup.
det = rows("detection.csv")
base = rows("baseline.csv")
if det:
    scen = [s for s in ORDER if any(r["scenario"] == s for r in det)]
    cols = ["CloudNativePG status", "Plain restore CronJob", "restore-check"]
    fig, ax = plt.subplots(figsize=(11.5, 0.6 * len(scen) + 1.4))
    for i, s in enumerate(scen):
        y = len(scen) - 1 - i
        rs = [r for r in det if r["scenario"] == s]
        n = len(rs)
        cnpg_flag = sum(1 for r in rs if not (r["cnpg_backup_phase"] == "completed" and r["cnpg_continuous_archiving"] == "True"))
        cells = [(cnpg_flag == 0, f"all OK  ({n}/{n})" if cnpg_flag == 0 else f"flags a problem  ({cnpg_flag}/{n})")]
        bs = [r for r in base if r["scenario"] == s]
        if bs:
            bfail = sum(1 for r in bs if r["cronjob_result"] != "pass")
            cells.append((bfail == 0, f"restore OK  ({len(bs)}/{len(bs)})" if bfail == 0 else f"restore failed  ({bfail}/{len(bs)})"))
        else:
            cells.append((None, "not run"))
        rc_fail = [r for r in rs if r["restorecheck_result"] != "Succeeded"]
        reasons = sorted({r["restorecheck_reason"] or r["restorecheck_result"] for r in rc_fail})
        cells.append((not rc_fail, f"Succeeded  ({n}/{n})" if not rc_fail else f"{', '.join(reasons)}  ({len(rc_fail)}/{n})"))
        for x, (ok_, txt) in enumerate(cells):
            color = MUTED if ok_ is None else (OK if ok_ else BAD)
            ax.add_patch(plt.Rectangle((x + 0.03, y + 0.08), 0.94, 0.84, color=color, alpha=0.16))
            ax.text(x + 0.5, y + 0.5, txt, ha="center", va="center", fontsize=9, color=color, fontweight="bold")
        ax.text(-0.04, y + 0.5, LABEL[s], ha="right", va="center", fontsize=10, color=INK)
    ax.set_xlim(0, len(cols)); ax.set_ylim(0, len(scen)); ax.axis("off")
    for x, c in enumerate(cols):
        ax.text(x + 0.5, len(scen) + 0.15, c, ha="center", fontweight="bold", color=INK)
    ax.set_title("Green means \"looks fine\". Only the healthy row should be green.", loc="left", x=-0.4, pad=26)
    save(fig, "detection-matrix.png")

    # 2. Time from the start of a check to its verdict, per scenario.
    fig, ax = plt.subplots(figsize=(8.5, 0.5 * len(scen) + 1))
    for i, s in enumerate(scen):
        v = [f(r["verdict_s"]) for r in det if r["scenario"] == s and f(r["verdict_s"]) is not None]
        if not v:
            continue
        m = statistics.median(v) / 60
        y = len(scen) - 1 - i
        failed = any(r["restorecheck_result"] == "Failed" for r in det if r["scenario"] == s)
        ax.barh(y, m, color=(BAD if failed else OK), alpha=0.85, height=0.6)
        ax.plot([min(v) / 60, max(v) / 60], [y, y], color=INK, lw=1.2)
        ax.text(max(v) / 60 + 0.15, y, f"{m:.1f} min", va="center", fontsize=9, color=INK)
    ax.set_yticks(range(len(scen))); ax.set_yticklabels([LABEL[s] for s in reversed(scen)])
    ax.set_xlabel("minutes from check start to verdict (bar: median, line: min to max)")
    ax.grid(axis="x", color=GRID); ax.set_axisbelow(True)
    ax.set_title("How long until you know", loc="left")
    save(fig, "time-to-verdict.png")

# 3. Restore time versus database size.
size = rows("size.csv")
if size:
    fig, ax = plt.subplots(figsize=(7, 4))
    xs = [f(r["db_gb"]) for r in size]; ys = [f(r["recovery_s"]) / 60 for r in size]
    ax.scatter(xs, ys, color=ACCENT, s=36, zorder=3, label="restore (each run)")
    bk = [f(r["backup_s"]) / 60 for r in size if f(r["backup_s"]) is not None]
    ax.scatter([f(r["db_gb"]) for r in size if f(r["backup_s"]) is not None], bk, color=MUTED, marker="^", s=30, zorder=3, label="base backup (each run)")
    by = {}
    for r in size:
        by.setdefault(r["scale"], []).append((f(r["db_gb"]), f(r["recovery_s"]) / 60))
    pts = sorted((statistics.median(p[0] for p in v), statistics.median(p[1] for p in v)) for v in by.values())
    ax.plot([p[0] for p in pts], [p[1] for p in pts], color=ACCENT, lw=1.5)
    for r in size:   # label runs more than twice their size's median (explained in BENCHMARK.md)
        med = statistics.median(f(x["recovery_s"]) for x in size if x["scale"] == r["scale"])
        if f(r["recovery_s"]) > 2 * med:
            ax.annotate("outlier: ran while the disk was filling (see notes)", (f(r["db_gb"]), f(r["recovery_s"]) / 60),
                        xytext=(-12, -4), textcoords="offset points", ha="right", va="center", fontsize=8, color=MUTED)
    ax.set_xlabel("database size (GB)"); ax.set_ylabel("minutes")
    ax.set_xlim(left=0); ax.set_ylim(bottom=0); ax.grid(color=GRID); ax.set_axisbelow(True); ax.legend(frameon=False)
    ax.set_title("Restore time grows with database size", loc="left")
    save(fig, "restore-vs-size.png")

# 4. Restore time versus WAL to replay.
wal = rows("walreplay.csv")
if wal:
    fig, ax = plt.subplots(figsize=(7, 4))
    base = [r for r in size if r["scale"] == "70"]   # the same 1 GB base with no WAL to replay
    ax.scatter([0.0] * len(base), [f(r["recovery_s"]) / 60 for r in base], color=MUTED, s=30, zorder=3, label="no WAL after backup")
    ax.scatter([f(r["wal_gb_after_backup"]) for r in wal], [f(r["recovery_s"]) / 60 for r in wal], color=WARN, s=36, zorder=3, label="after 1, 3 or 6 min of pgbench")
    ax.legend(frameon=False, loc="upper left")
    ax.set_xlabel("WAL written after the base backup (GB)"); ax.set_ylabel("restore minutes")
    ax.set_xlim(left=-0.1); ax.set_ylim(bottom=0); ax.grid(color=GRID); ax.set_axisbelow(True)
    ax.set_title("Restore time grows with WAL to replay (1 GB base)", loc="left")
    save(fig, "restore-vs-wal.png")

# 5. Restore time against WAL segments to replay, and back-to-back runs.
rel = rows("reliability.csv")


def slope(pts):
    n = len(pts); mx = sum(p[0] for p in pts) / n; my = sum(p[1] for p in pts) / n
    return sum((p[0] - mx) * (p[1] - my) for p in pts) / sum((p[0] - mx) ** 2 for p in pts)


if rel and wal and size:
    r1 = next(r for r in rel if r["run"] == "1")
    rp = [(f(r["recovery_end_segment"]) - f(r1["recovery_end_segment"]), f(r["recovery_s"]) - f(r1["recovery_s"])) for r in rel]
    ref = [r for r in size if r["scale"] == "70"]
    seg0 = statistics.median(f(r["recovery_end_segment"]) for r in ref); sec0 = statistics.median(f(r["recovery_s"]) for r in ref)
    wp = [(f(r["recovery_end_segment"]) - seg0, f(r["recovery_s"]) - sec0) for r in wal]
    fig, ax = plt.subplots(figsize=(7, 4.2))
    ax.scatter([p[0] for p in rp], [p[1] for p in rp], color=ACCENT, s=30, zorder=3,
               label=f"30 back-to-back runs, idle writer: {slope(rp):.2f} s per segment")
    ax.scatter([p[0] for p in wp], [p[1] for p in wp], color=WARN, s=36, marker="D", zorder=3,
               label=f"pgbench after the backup: {slope(wp):.2f} s per segment")
    ax.set_xlabel("extra WAL segments to replay (16 MiB each)"); ax.set_ylabel("extra restore seconds")
    ax.set_xlim(left=0); ax.set_ylim(bottom=0); ax.grid(color=GRID); ax.set_axisbelow(True); ax.legend(frameon=False, loc="upper left")
    ax.set_title("Every WAL segment costs about a second, full or nearly empty", loc="left")
    save(fig, "restore-vs-segments.png")

if rel:
    mem = [(int(r["run"]), f(r["operator_memory_mib"])) for r in rel if f(r["operator_memory_mib"]) is not None]
    if mem:
        fig, ax = plt.subplots(figsize=(7, 3))
        ax.plot([m[0] for m in mem], [m[1] for m in mem], color=ACCENT, marker="o", ms=3)
        ax.set_xlabel("run"); ax.set_ylabel("operator memory (MiB)"); ax.set_ylim(bottom=0)
        ax.grid(color=GRID); ax.set_axisbelow(True)
        ok = sum(1 for r in rel if r["result"] == "Succeeded")
        ax.set_title(f"{len(rel)} back-to-back runs: {ok} succeeded, operator memory flat", loc="left")
        save(fig, "operator-memory.png")
