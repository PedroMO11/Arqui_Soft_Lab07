"""Experimento baseline vs. propuesta en 3 escenarios.

Estrategias (flags de la api):
  baseline           1 llamada por servicio
  hedged-p95-b10     Modo A, 2da copia al p95 observado, budget +10%
  double             Modo B, 2 copias desde t=0
  double+hedge-p95   propuesta, 2 copias desde t=0 y 3ra al p95

Escenarios:
  R   lentitud por request (2% tarda 1 a 1.5 s), los 10 servicios son lectura
  RA  igual que R pero S10 es una accion y nunca se duplica
  E   episodios, S7 y S8 se degradan por completo ~1.5 s cada ~20 s

Cada corrida levanta procesos limpios (blackbox + api) y dispara carga
open-loop con loadgen. Misma semilla por repeticion para todas las estrategias.

Uso:
    python scripts/run_experiments.py              # experimento completo (~32 min)
    python scripts/run_experiments.py --resume     # continua uno cortado
    python scripts/run_experiments.py --quick      # 10 s por corrida, 1 rep
"""

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BIN = ROOT / "bin"
EXE = ".exe" if os.name == "nt" else ""
RESULTS = ROOT / "results"
RUNS = RESULTS / "runs"

MAX_LOAD = 3.0  # el profesor acepta duplicar o triplicar la carga sobre los servicios

# nombre -> flags de la api
STRATEGIES = {
    "baseline": ["-initial", "1", "-copies", "1"],
    "hedged-p95-b10": ["-copies", "2", "-hedge-pct", "95", "-budget", "0.1"],
    "double": ["-initial", "2", "-copies", "2"],
    "double+hedge-p95": ["-initial", "2", "-copies", "3", "-hedge-pct", "95", "-budget", "2.0"],
}
RECOMMENDED = "double+hedge-p95"

# escenario -> (flags del blackbox, flags extra de la api)
SCENARIOS = {
    "R": (["-profile", "spec", "-scenario", "request"], ["-actions", ""]),
    "RA": (["-profile", "spec", "-scenario", "request"], ["-actions", "S10"]),
    "E": (["-profile", "spec", "-scenario", "episode"], ["-actions", ""]),
}


def build():
    subprocess.run(["go", "build", "-o", str(BIN) + os.sep, "./cmd/..."], cwd=ROOT, check=True)


def wait_ok(url, timeout=10.0):
    end = time.time() + timeout
    while time.time() < end:
        try:
            with urllib.request.urlopen(url, timeout=1) as r:
                if r.status == 200:
                    return
        except OSError:
            time.sleep(0.1)
    raise RuntimeError(f"no responde: {url}")


def run_one(scenario, name, seed, rep, args, keep_csv):
    tag = f"{scenario}_{name}_r{rep}"
    bb_flags, api_extra = SCENARIOS[scenario]
    bb = subprocess.Popen([str(BIN / f"blackbox{EXE}"), "-seed", str(seed)] + bb_flags,
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    api_cmd = [str(BIN / f"api{EXE}"), "-name", name] + STRATEGIES[name] + api_extra
    if keep_csv:
        api_cmd += ["-log", str(RUNS / f"{tag}_api.csv")]
    api = subprocess.Popen(api_cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        wait_ok("http://127.0.0.1:9000/healthz")
        wait_ok("http://127.0.0.1:8080/healthz")
        cmd = [
            str(BIN / f"loadgen{EXE}"),
            "-url", "http://127.0.0.1:8080/aggregate",
            "-rps", str(args.rps),
            "-duration", f"{args.duration}s",
            "-warmup", f"{args.warmup}s",
            "-label", tag,
            "-summary", str(RUNS / f"{tag}.json"),
        ]
        if keep_csv:
            cmd += ["-out", str(RUNS / f"{tag}_requests.csv")]
        subprocess.run(cmd, check=True, stdout=subprocess.DEVNULL)
        with urllib.request.urlopen("http://127.0.0.1:9000/stats", timeout=2) as r:
            bb_stats = json.load(r)
        with urllib.request.urlopen("http://127.0.0.1:8080/stats", timeout=2) as r:
            api_stats = json.load(r)
    finally:
        api.kill()
        bb.kill()
        api.wait()
        bb.wait()
        time.sleep(0.5)  # libera puertos

    summary = json.loads((RUNS / f"{tag}.json").read_text())
    summary.update({
        "scenario": scenario, "config": name, "rep": rep, "seed": seed,
        "blackbox_received_total": bb_stats["total_received"],
        "blackbox": {k: v for k, v in bb_stats.items() if k.startswith("S")},
        "hedge_delay_ms": api_stats.get("hedge_delay_ms"),
    })
    (RUNS / f"{tag}.json").write_text(json.dumps(summary, indent=2))
    return summary


def run_matrix(matrix, args, t0):
    total = len(matrix) * args.reps
    done = 0
    for rep in range(1, args.reps + 1):
        seed = 1000 + rep
        for scenario, name in matrix:
            done += 1
            prev = RUNS / f"{scenario}_{name}_r{rep}.json"
            if args.resume and prev.exists() and "scenario" in json.loads(prev.read_text()):
                continue
            s = run_one(scenario, name, seed, rep, args, keep_csv=(rep == 1))
            print(f"[{done}/{total}] {scenario:2s} {name:20s} r{rep}  p50={s['p50_ms']:7.1f}  "
                  f"p95={s['p95_ms']:7.1f}  p99={s['p99_ms']:7.1f}  p99.9={s['p999_ms']:7.1f}  "
                  f"load=x{s['load_multiplier']:.2f}  err={s['errors']}  ({time.time()-t0:.0f}s)",
                  flush=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--rps", type=float, default=150)
    ap.add_argument("--duration", type=int, default=40)
    ap.add_argument("--warmup", type=int, default=10)
    ap.add_argument("--reps", type=int, default=3)
    ap.add_argument("--quick", action="store_true")
    ap.add_argument("--resume", action="store_true", help="salta corridas que ya tienen resumen")
    args = ap.parse_args()
    if args.quick:
        args.duration, args.warmup, args.reps = 10, 3, 1

    RUNS.mkdir(parents=True, exist_ok=True)
    build()
    t0 = time.time()

    run_matrix([(sc, n) for sc in SCENARIOS for n in STRATEGIES], args, t0)

    meta = {"rps": args.rps, "duration_s": args.duration, "warmup_s": args.warmup,
            "reps": args.reps, "max_load": MAX_LOAD, "recommended": RECOMMENDED}
    (RESULTS / "experiment_meta.json").write_text(json.dumps(meta, indent=2))
    subprocess.run([sys.executable, str(ROOT / "scripts" / "report.py")], check=True)


if __name__ == "__main__":
    main()
