"""Agrega las corridas de results/runs y genera results/RESULTS.md + graficas.

Cada metrica es la mediana de las repeticiones. Para p99 tambien se muestra
el rango [min-max] entre repeticiones. Requests y llamadas son la suma.
"""

import csv
import json
import statistics
from collections import defaultdict
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
RESULTS = ROOT / "results"
RUNS = RESULTS / "runs"

SCENARIOS = {
    "R": "R. Lentitud por request, los 10 servicios son lectura",
    "RA": "RA. Igual que R pero S10 es una acción y nunca se duplica",
    "E": "E. Episodios, S7 y S8 se degradan por completo ~1.5 s cada ~20 s",
}
# estrategia -> titulo en las tablas
STRATEGIES = {
    "baseline": "Baseline",
    "hedged-p95-b10": "Modo A (hedged-p95)",
    "double": "Modo B (double)",
    "double+hedge-p95": "Propuesta (double+hedge-p95)",
}
RECOMMENDED = "double+hedge-p95"

INK, INK2, GRID = "#0b0b0b", "#52514e", "#e4e3df"
# paleta categorica de referencia, en orden fijo; el color sigue a la estrategia
COLORS = {
    "baseline": "#eb6834",
    "hedged-p95-b10": "#6250d6",
    "double": "#2a78d6",
    "double+hedge-p95": "#1baf7a",
}
METRICS = ["p50_ms", "p95_ms", "p99_ms", "p999_ms", "max_ms", "mean_ms"]


def load_runs():
    runs = defaultdict(list)
    for p in sorted(RUNS.glob("*_r*.json")):
        d = json.loads(p.read_text())
        if "scenario" in d and d["config"] in STRATEGIES:
            runs[(d["scenario"], d["config"])].append(d)
    return runs


def agg(rs):
    out = {m: statistics.median(r[m] for r in rs) for m in METRICS}
    out["p99_min"] = min(r["p99_ms"] for r in rs)
    out["p99_max"] = max(r["p99_ms"] for r in rs)
    out["reps"] = len(rs)
    for k in ("requests", "errors", "downstream_calls", "hedges", "hedge_wins"):
        out[k] = sum(r[k] for r in rs)
    out["load"] = statistics.median(r["load_multiplier"] for r in rs)
    out["slow_pct"] = statistics.median(r["slow_over_100ms_pct"] for r in rs)
    out["rps"] = statistics.median(r["achieved_rps"] for r in rs)
    return out


def fmt_ms(x):
    return f"{x:,.1f} ms" if x < 1000 else f"{x / 1000:,.2f} s"


def delta(a, b):
    return f"{(b - a) / a * 100:+.0f}%" if a else "-"


def challenge_table(cols):
    """cols = [(titulo, agg)], la primera columna es el baseline."""
    head = "| Métrica | " + " | ".join(t for t, _ in cols) + " |"
    sep = "|---|" + "---:|" * len(cols)
    base = cols[0][1]
    rows = [head, sep]
    for key, label in [("p50_ms", "p50"), ("p95_ms", "p95"), ("p99_ms", "p99"), ("p999_ms", "p99.9")]:
        cells = [fmt_ms(base[key])] + [f"{fmt_ms(a[key])} ({delta(base[key], a[key])})" for _, a in cols[1:]]
        rows.append(f"| {label} | " + " | ".join(cells) + " |")
    rows.append("| p99 [min-max entre repeticiones] | " + " | ".join(
        f"{a['p99_min']:.0f}-{a['p99_max']:.0f} ms" for _, a in cols) + " |")
    rows.append("| Requests ejecutadas (cliente → API) | " + " | ".join(f"{a['requests']:,}" for _, a in cols) + " |")
    rows.append("| Llamadas internas a servicios (costo) | " + " | ".join(f"{a['downstream_calls']:,}" for _, a in cols) + " |")
    rows.append("| Carga sobre los servicios | " + " | ".join(f"x{a['load']:.2f}" for _, a in cols) + " |")
    rows.append("| Requests > 100 ms | " + " | ".join(f"{a['slow_pct']:.2f}%" for _, a in cols) + " |")
    rows.append("| Errores | " + " | ".join(str(a["errors"]) for _, a in cols) + " |")
    return "\n".join(rows)


def read_latencies(scenario, config, warmup_ms):
    p = RUNS / f"{scenario}_{config}_r1_requests.csv"
    if not p.exists():
        return None
    out = []
    with p.open() as f:
        for row in csv.DictReader(f):
            if int(row["offset_ms"]) >= warmup_ms:
                out.append((int(row["offset_ms"]), float(row["latency_ms"])))
    return out


def style(ax):
    for s in ("top", "right"):
        ax.spines[s].set_visible(False)
    for s in ("left", "bottom"):
        ax.spines[s].set_color(GRID)
    ax.tick_params(colors=INK2, labelsize=9)
    ax.grid(True, color=GRID, linewidth=0.8)
    ax.set_axisbelow(True)


def ccdf(plt, scenario, title, fname, warmup_ms):
    fig, ax = plt.subplots(figsize=(8, 4.6), dpi=150)
    drawn = 0
    for cfg, label in STRATEGIES.items():
        lat = read_latencies(scenario, cfg, warmup_ms)
        if not lat:
            continue
        xs = sorted(l for _, l in lat)
        n = len(xs)
        ys = [(n - i) / n for i in range(n)]
        ax.step(xs, ys, where="post", color=COLORS[cfg], linewidth=2, label=label)
        drawn += 1
    if not drawn:
        plt.close(fig)
        return None
    ax.set_xscale("log")
    ax.set_yscale("log")
    ax.set_ylim(1 / 20000, 1.2)
    for p, lbl in [(0.5, "p50"), (0.05, "p95"), (0.01, "p99"), (0.001, "p99.9")]:
        ax.axhline(p, color=INK2, linewidth=0.6, linestyle=":")
        ax.text(0.995, p, lbl, transform=ax.get_yaxis_transform(), color=INK2, fontsize=8,
                ha="right", va="bottom")
    ax.set_xlabel("latencia de la request (ms, escala log)")
    ax.set_ylabel("fracción de requests más lentas que x")
    ax.set_title(title, loc="left", fontsize=12, color=INK)
    style(ax)
    ax.legend(frameon=False, fontsize=9, loc="lower left")
    fig.tight_layout()
    fig.savefig(RESULTS / fname)
    plt.close(fig)
    return fname


def charts(agg_by, warmup_ms):
    try:
        import matplotlib
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt
    except ImportError:
        print("matplotlib no instalado, se omiten las graficas")
        return []
    plt.rcParams.update({"font.family": "DejaVu Sans", "text.color": INK, "axes.labelcolor": INK2})
    made = []
    for sc, title, fname in [("R", "Escenario R: cola de latencia (CCDF)", "ccdf_R.png"),
                             ("RA", "Escenario RA: con S10 como acción el p99 queda acotado por S10", "ccdf_RA.png")]:
        m = ccdf(plt, sc, title, fname, warmup_ms)
        if m:
            made.append(m)

    # beneficio (p99 y p99.9) vs. costo (carga), escenario R
    pts = [(cfg, agg_by[("R", cfg)]) for cfg in STRATEGIES if ("R", cfg) in agg_by]
    if pts:
        fig, ax = plt.subplots(figsize=(8, 4.8), dpi=150)
        ax.axvspan(3.0, 3.2, color="#f3d9d9", alpha=0.6, linewidth=0)
        ax.text(3.01, 0.97, "límite\n3x", transform=ax.get_xaxis_transform(), fontsize=8, color=INK2, va="top")
        for cfg, a in pts:
            ax.plot([a["load"], a["load"]], [a["p99_ms"], a["p999_ms"]], color=COLORS[cfg], linewidth=1.5, alpha=0.5)
            ax.plot(a["load"], a["p99_ms"], "o", color=COLORS[cfg], markersize=9,
                    markeredgecolor="white", markeredgewidth=1.5)
            ax.plot(a["load"], a["p999_ms"], "s", color=COLORS[cfg], markersize=7,
                    markeredgecolor="white", markeredgewidth=1.5)
            ax.annotate(STRATEGIES[cfg], (a["load"], a["p99_ms"]), xytext=(8, -4), textcoords="offset points",
                        fontsize=8, color=INK2)
        ax.set_yscale("log")
        ax.set_xlim(0.9, 3.2)
        ax.set_xlabel("carga sobre los servicios (llamadas por request ÷ 10)")
        ax.set_ylabel("latencia (ms, escala log)")
        ax.set_title("Escenario R: beneficio vs. costo (círculo = p99, cuadrado = p99.9)", loc="left",
                     fontsize=12, color=INK)
        style(ax)
        fig.tight_layout()
        fig.savefig(RESULTS / "tradeoff_R.png")
        plt.close(fig)
        made.append("tradeoff_R.png")

    # timeline del escenario E
    lb = read_latencies("E", "baseline", warmup_ms)
    lt = read_latencies("E", RECOMMENDED, warmup_ms)
    if lb and lt:
        fig, axes = plt.subplots(2, 1, figsize=(8, 5.2), dpi=150, sharex=True, sharey=True)
        for ax, cfg, lat in zip(axes, ("baseline", RECOMMENDED), (lb, lt)):
            ax.scatter([o / 1000 for o, _ in lat], [l for _, l in lat], s=4, color=COLORS[cfg], alpha=0.6,
                       linewidths=0)
            ax.set_yscale("log")
            ax.set_ylabel("latencia (ms)")
            ax.set_title(STRATEGIES[cfg], loc="left", fontsize=10, color=INK)
            style(ax)
        axes[1].set_xlabel("tiempo desde el inicio (s)")
        fig.suptitle("Escenario E: durante un episodio todas las copias son lentas", x=0.02, ha="left",
                     fontsize=12, color=INK)
        fig.tight_layout()
        fig.savefig(RESULTS / "timeline_E.png")
        plt.close(fig)
        made.append("timeline_E.png")
    return made


def main():
    meta_p = RESULTS / "experiment_meta.json"
    meta = json.loads(meta_p.read_text()) if meta_p.exists() else {"warmup_s": 10}
    runs = load_runs()
    agg_by = {k: agg(v) for k, v in runs.items()}

    md = ["# Resultados del experimento", ""]
    md.append(f"Carga open-loop de **{meta.get('rps', '?')} req/s**, {meta.get('duration_s', '?')} s medidos por "
              f"corrida (+{meta.get('warmup_s', '?')} s de warm-up), **{meta.get('reps', '?')} repeticiones** por "
              "configuración con semillas distintas. Cada métrica es la mediana de las repeticiones, las requests "
              f"y llamadas son la suma. Restricción de costo: carga ≤ {meta.get('max_load', 3)}x sobre los servicios.")
    md.append("")
    md += [f"**Propuesta:** {RECOMMENDED}. Cada lectura sale con 2 copias en t = 0 y, si ninguna respondió al p95 "
           "observado del servicio, se manda una 3ra.", ""]

    for sc, title in SCENARIOS.items():
        cols = [(label, agg_by[(sc, cfg)]) for cfg, label in STRATEGIES.items() if (sc, cfg) in agg_by]
        if not cols:
            continue
        md += [f"## {title}", "", challenge_table(cols), ""]

    made = charts(agg_by, meta.get("warmup_s", 10) * 1000)
    if made:
        md += ["## Gráficas", ""] + [f"![{m}]({m})" for m in made] + [""]

    (RESULTS / "RESULTS.md").write_text("\n".join(md), encoding="utf-8")
    (RESULTS / "summary.json").write_text(
        json.dumps({f"{s}/{c}": a for (s, c), a in agg_by.items()}, indent=2), encoding="utf-8")
    print(f"escrito {RESULTS / 'RESULTS.md'}")


if __name__ == "__main__":
    main()
