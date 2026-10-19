"""Genera diagramas/arquitectura-hedged-v2.excalidraw.

Copia el board original del equipo (diagramas/arquitectura-hedged.excalidraw)
sin modificarlo y agrega debajo la vista de componentes, el trade-off, los
resultados y la decision. Los numeros salen de poc/results/summary.json y la
estrategia elegida de experiment_meta.json, asi el diagrama nunca queda
desalineado con el experimento. La decision se lee de results/decision.txt.

    python scripts/build_diagram.py
"""

import json
import random
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
REPO = ROOT.parent
BASE_BOARD = REPO / "diagramas" / "arquitectura-hedged.excalidraw"
OUT = REPO / "diagramas" / "arquitectura-hedged-v2.excalidraw"
SUMMARY = ROOT / "results" / "summary.json"
META_PATH = ROOT / "results" / "experiment_meta.json"
DECISION_PATH = ROOT / "results" / "decision.txt"
BOARD_OFFSET_Y = 1750  # debajo del board original (que llega a y ~1580)

INK = "#1e1e1e"
BLUE_FILL = "#d0e4fb"
BLUE = "#1c5cab"
GRAY_FILL = "#f1f0ec"
FLAKY_FILL = "#fde0d2"
FLAKY = "#c4461a"
OBS_FILL = "#d3f1e5"
TAG = "#c92a2a"
NOTE_FILL = "#fff8db"
ACTION_FILL = "#fff3bf"
ACTION = "#a5790a"

FONT = 2  # Helvetica, mas legible que Virgil al exportar
rnd = random.Random(10)
elements = []
_ids = 0


def nid(prefix):
    global _ids
    _ids += 1
    return f"{prefix}{_ids}"


def _base(kind, x, y, w, h, **kw):
    e = {
        "id": kw.pop("id", nid(kind[:2])),
        "type": kind,
        "x": x, "y": y, "width": w, "height": h,
        "angle": 0,
        "strokeColor": kw.pop("stroke", INK),
        "backgroundColor": kw.pop("fill", "transparent"),
        "fillStyle": "solid",
        "strokeWidth": kw.pop("sw", 2),
        "strokeStyle": kw.pop("style", "solid"),
        "roughness": kw.pop("rough", 1),
        "opacity": 100,
        "groupIds": [],
        "frameId": None,
        "roundness": kw.pop("roundness", {"type": 3}),
        "seed": rnd.randint(1, 2**31),
        "version": 1,
        "versionNonce": rnd.randint(1, 2**31),
        "isDeleted": False,
        "boundElements": [],
        "updated": 1760400000000,
        "link": None,
        "locked": False,
    }
    e.update(kw)
    elements.append(e)
    return e


def _text_size(text, size):
    lines = text.split("\n")
    return max(len(l) for l in lines) * size * 0.56, len(lines) * size * 1.25


def text(x, y, s, size=20, color=INK, align="left", container=None, w=None, h=None):
    tw, th = _text_size(s, size)
    e = _base("text", x, y, w or tw, h or th, stroke=color, roundness=None, sw=1,
              text=s, originalText=s, fontSize=size, fontFamily=FONT, textAlign=align,
              verticalAlign="middle" if container else "top",
              containerId=container["id"] if container else None,
              autoResize=True, lineHeight=1.25)
    if container:
        container["boundElements"].append({"type": "text", "id": e["id"]})
    return e


def box(x, y, w, h, label, fill="transparent", stroke=INK, size=20, style="solid", kind="rectangle", round_=True):
    b = _base(kind, x, y, w, h, fill=fill, stroke=stroke, style=style,
              roundness=({"type": 3} if round_ else None) if kind == "rectangle" else {"type": 2})
    if label:
        tw, th = _text_size(label, size)
        text(x + (w - tw) / 2, y + (h - th) / 2, label, size=size, align="center", container=b, w=tw, h=th)
    return b


def frame(x, y, w, h, title, fill="transparent", stroke=INK, style="dashed", size=22):
    b = _base("rectangle", x, y, w, h, fill=fill, stroke=stroke, style=style, sw=2, roundness=None)
    text(x + 16, y + 10, title, size=size, color=stroke)
    return b


def _edge(b, toward):
    """Punto del borde de b en direccion a `toward` (centro de otra caja)."""
    cx, cy = b["x"] + b["width"] / 2, b["y"] + b["height"] / 2
    dx, dy = toward[0] - cx, toward[1] - cy
    if dx == 0 and dy == 0:
        return cx, cy
    sx = (b["width"] / 2) / abs(dx) if dx else float("inf")
    sy = (b["height"] / 2) / abs(dy) if dy else float("inf")
    s = min(sx, sy)
    return cx + dx * s, cy + dy * s


def center(b):
    return b["x"] + b["width"] / 2, b["y"] + b["height"] / 2


def arrow(a, b, label=None, style="solid", color=INK, size=16, start=None, end=None, head="arrow"):
    p0 = start or _edge(a, end or center(b))
    p1 = end or _edge(b, start or center(a))
    e = _base("arrow", p0[0], p0[1], abs(p1[0] - p0[0]), abs(p1[1] - p0[1]),
              stroke=color, style=style, roundness={"type": 2},
              points=[[0, 0], [p1[0] - p0[0], p1[1] - p0[1]]],
              lastCommittedPoint=None,
              startBinding={"elementId": a["id"], "focus": 0, "gap": 6},
              endBinding={"elementId": b["id"], "focus": 0, "gap": 6},
              startArrowhead=None, endArrowhead=head, elbowed=False)
    a["boundElements"].append({"type": "arrow", "id": e["id"]})
    b["boundElements"].append({"type": "arrow", "id": e["id"]})
    if label:
        tw, th = _text_size(label, size)
        mx, my = (p0[0] + p1[0]) / 2, (p0[1] + p1[1]) / 2
        text(mx - tw / 2, my - th / 2, label, size=size, color=color, align="center",
             container=e, w=tw, h=th)
    return e


def tag(x, y, kind, body, target):
    """Etiqueta SPOF/Bottleneck pegada al nodo y unida con flecha."""
    s = f"{kind}\n{body}"
    tw, th = _text_size(s, 17)
    t = box(x, y, tw + 24, th + 18, s, fill="#ffe3e3", stroke=TAG, size=17, round_=False)
    arrow(t, target, color=TAG, style="dashed")
    return t


# ---------------------------------------------------------------- resultados
def load_json(p):
    return json.loads(p.read_text(encoding="utf-8")) if p.exists() else {}


def ms(x):
    if x >= 1000:
        return f"{x / 1000:.2f} s"
    return f"{x:,.0f} ms" if x >= 10 else f"{x:.1f} ms"


S = load_json(SUMMARY)
META = load_json(META_PATH)
WIN = META.get("recommended") or "double+hedge-p95"
DECISION = DECISION_PATH.read_text(encoding="utf-8").strip() if DECISION_PATH.exists() else "pendiente de resultados"


def m(key, metric, default=0):
    v = S.get(key, {}).get(metric)
    return default if v is None else v


STRAT_DESC = {
    "double+hedge-p95": "2 copias en t = 0, 3ra al p95",
}

# ---------------------------------------------------------------- 4. componentes
X0, OY = 0, BOARD_OFFSET_Y
frame(X0, OY, 2300, 1010, "4. Arquitectura de componentes de la propuesta (solo cambia la API, los servicios siguen siendo black box)",
      stroke=BLUE)

cli = box(X0 + 40, OY + 230, 150, 90, "Cliente", fill="#ffffff", kind="ellipse")
lg = box(X0 + 30, OY + 420, 175, 110, "Loadgen open-loop\n+ Locust\n150 req/s", fill="#ffffff", size=17)

apiF = frame(X0 + 270, OY + 70, 700, 770, "API Aggregator (Go, stateless)", fill="#f5f9ff", stroke=BLUE, style="solid")
orch = box(X0 + 300, OY + 130, 640, 80, "Fan-out Orchestrator\n10 goroutines, responde cuando convergen las 10", fill=BLUE_FILL, stroke=BLUE, size=18)
hedge = box(X0 + 300, OY + 260, 640, 105,
            f"Hedging Client (por servicio)\nestrategia elegida: {WIN}\n{STRAT_DESC.get(WIN, '')}, gana la primera respuesta",
            fill=BLUE_FILL, stroke=BLUE, size=17)
trk = box(X0 + 300, OY + 430, 150, 130, "Latency Tracker\npXX por servicio\nventana 1000", fill="#ffffff", stroke=BLUE, size=15)
bud = box(X0 + 463, OY + 430, 150, 130, "Hedge Budget\ntoken bucket\ncarga máx 3x", fill="#ffffff", stroke=BLUE, size=15)
grd = box(X0 + 626, OY + 430, 150, 130, "Action Guard\nuna acción\nnunca se duplica", fill=ACTION_FILL, stroke=ACTION, size=15)
cnl = box(X0 + 789, OY + 430, 150, 130, "Cancel\ncontext\nWithCancel", fill="#ffffff", stroke=BLUE, size=15)
alog = box(X0 + 300, OY + 650, 640, 90, "Async Logger\nbuffer en memoria, si se llena descarta\nel logging nunca bloquea la request", fill=OBS_FILL, size=17)

arrow(cli, orch, "GET /aggregate", start=(X0 + 190, OY + 275), end=(X0 + 300, OY + 170))
arrow(lg, apiF, "carga de prueba", start=(X0 + 205, OY + 475), end=(X0 + 270, OY + 475))
arrow(orch, hedge, "1 llamada lógica por servicio", size=15)
arrow(trk, hedge, "delay = pXX", size=14)
arrow(bud, hedge, "¿hay token?", size=14)
arrow(grd, hedge, "¿es acción?", size=14)
arrow(hedge, cnl, "al llegar\nla primera", size=14, start=(X0 + 864, OY + 365), end=(X0 + 864, OY + 430))
arrow(orch, alog, "1 fila\npor request", start=(X0 + 290, OY + 200), end=(X0 + 295, OY + 670), style="dotted", color="#2b8a6e", size=14)

bbF = frame(X0 + 1110, OY + 70, 490, 770, "Servicios externos (black box,\nno modificables)", fill="#fafaf8", stroke="#52514e", size=18)
svc = {}
for i in range(10):
    col_, row = i % 2, i // 2
    name = f"S{i + 1}"
    is_action = False  # caso principal: los 10 son lectura (S10 como accion es el escenario RA)
    svc[name] = box(X0 + 1140 + col_ * 230, OY + 165 + row * 120, 200, 80,
                    f"{name}  {'acción' if is_action else 'lectura'}",
                    fill=ACTION_FILL if is_action else "#ffffff", stroke=ACTION if is_action else INK, size=17)
text(X0 + 1135, OY + 775, "cada servicio responde en 10 a 30 ms\ny el 2% de las llamadas tarda 1 a 1.5 s", size=15, color="#52514e")

arrow(hedge, bbF, "1. copia(s) en t = 0", start=(X0 + 940, OY + 290), end=(X0 + 1110, OY + 290), color=BLUE, size=15)
arrow(hedge, bbF, "2. otra copia si t > pXX", start=(X0 + 940, OY + 345), end=(X0 + 1110, OY + 405), style="dashed", color=BLUE, size=15)
arrow(cnl, bbF, "3. cancel", start=(X0 + 939, OY + 520), end=(X0 + 1110, OY + 545), style="dotted", color=BLUE, size=15)

logs = box(X0 + 300, OY + 880, 290, 100, "Logs transaccionales\nCSV, 1 fila por request\ncon latencia", fill=OBS_FILL, size=16)
rep = box(X0 + 650, OY + 880, 290, 100, "Report / Dashboard\np50 p95 p99 p99.9\ncarga xN, % copias", fill=OBS_FILL, size=16)
arrow(alog, logs, None)
arrow(logs, rep, "report.py")

tag(X0 + 20, OY + 640, "SPOF", "API Aggregator\n(1 instancia en el PoC)\nmitigación: stateless,\nN réplicas detrás de un LB", apiF)
ra_note = box(X0 + 1650, OY + 600, 560, 150,
              "Dato adicional (escenario RA)\nsi S10 fuera una acción, la Action Guard\nlo llama 1 sola vez y su 2% lento\nfija el p99 en ~1.24 s",
              fill=ACTION_FILL, stroke=ACTION, size=16, round_=False)
arrow(ra_note, svc["S10"], None, color=ACTION, style="dashed")
tag(X0 + 1650, OY + 200, "Bottleneck", "el servicio más lento de cada\nrequest define el critical path", bbF)

# ---------------------------------------------------------------- 5. notas
NY = OY + 1060
r_base, r_win = f"R/baseline", f"R/{WIN}"
notes = [
    ("Qué cambia",
     "Solo la API. Un Hedging Client manda a cada servicio de lectura una o más\n"
     "copias de la misma request, gana la primera respuesta y el resto se cancela.\n"
     "Una acción no idempotente recibe 1 sola llamada. La respuesta sigue esperando\n"
     "las 10 partes, por lo tanto converge completa."),
    ("Por qué mejora la tail latency",
     "Con p = 2% por servicio, 1 − 0.98¹⁰ = 18.3% de las requests espera a un lento.\n"
     "Si la lentitud es por request, 2 copias son lentas a la vez con p² = 0.04% y 3\n"
     "copias con p³ = 0.0008%. El fan-out deja de amplificar la cola porque cada\n"
     "servicio responde con la copia más rápida."),
    ("Trade-off (beneficio / costo)",
     f"Beneficio  p99 {ms(m(r_base, 'p99_ms'))} → {ms(m(r_win, 'p99_ms'))} con {WIN} (escenario R).\n"
     f"Costo  carga x{m(r_win, 'load'):.2f} sobre los servicios (límite aceptado 3x), más llamadas\n"
     "a terceros con rate limit o cobro por consulta, y más complejidad en la API\n"
     "(timers, cancelación, budget, p95 por servicio)."),
    ("Cuándo no ayuda",
     f"Acciones  si S10 fuera acción su cola no se recorta, p99 {ms(m(f'RA/{WIN}', 'p99_ms'))} (escenario RA).\n"
     f"Episodios  si S7 o S8 están degradados todas sus copias son lentas, p99 {ms(m(f'E/{WIN}', 'p99_ms'))}.\n"
     "En ambos casos la copia extra no empeora la latencia, y el budget limita\n"
     "la carga a 3x mientras dura el problema."),
]
for i, (title, body) in enumerate(notes):
    cx = X0 + (i % 2) * 1160
    cy = NY + (i // 2) * 250
    _base("rectangle", cx, cy, 1140, 230, fill=NOTE_FILL, stroke="#b08900", roundness=None)
    text(cx + 16, cy + 12, title, size=20, color="#7a5d00")
    text(cx + 16, cy + 50, body, size=17)

# ---------------------------------------------------------------- 6. resultados
RY = NY + 540
rows_spec = [
    ("Baseline", "1 llamada por servicio, espera lo que tarde", "baseline"),
    ("Modo A  hedged-p95", "1 llamada, 2da copia si no responde al p95", "hedged-p95-b10"),
    ("Modo B  double", "2 copias desde t = 0, gana la más rápida", "double"),
    ("Propuesta  double+hedge-p95", "2 copias desde t = 0 y 3ra al p95", WIN),
]
fh = 150 + 78 * len(rows_spec) + 40
frame(X0, RY, 2300, fh, "6. Resultados de las ideas finales (mediana de 3 repeticiones, 18 000 requests por estrategia, 150 req/s open-loop)", stroke=BLUE)
cols = [("Estrategia", 520), ("p50", 140), ("p95", 140), ("p99", 140), ("p99.9", 140), ("carga", 120),
        ("p99 si S10 es acción", 250), ("p99 con episodios", 230)]
tx, ty = X0 + 30, RY + 60
cx = tx
for name, w in cols:
    text(cx, ty, name, size=18, color=BLUE)
    cx += w
for r, (lbl, desc, key) in enumerate(rows_spec):
    cy = ty + 48 + r * 78
    best = key == WIN
    color = "#2b8a3e" if best else INK
    text(tx, cy, lbl, size=18, color=color)
    text(tx, cy + 26, desc, size=14, color="#52514e")
    vals = [ms(m(f"R/{key}", k)) for k in ("p50_ms", "p95_ms", "p99_ms", "p999_ms")] + \
           [f"x{m(f'R/{key}', 'load'):.2f}", ms(m(f"RA/{key}", "p99_ms")), ms(m(f"E/{key}", "p99_ms"))]
    cx = tx + cols[0][1]
    for (name, w), v in zip(cols[1:], vals):
        text(cx, cy + 6, v, size=18, color=color)
        cx += w
text(tx, RY + fh - 40, "Las variantes que esperan antes de mandar la copia (Modo A) tienen un piso de ~55 ms en el p95, "
     "que es el delay del p95 más la respuesta de la copia.", size=15, color="#52514e")

DY = RY + fh + 40
_base("rectangle", X0, DY, 2300, 420, fill="#e6f4ea", stroke="#2b8a3e", roundness=None)
text(X0 + 20, DY + 15, "7. Decisión", size=24, color="#2b8a3e")
text(X0 + 20, DY + 60, DECISION, size=17)


def main():
    base = load_json(BASE_BOARD)
    base_elements = base.get("elements", [])
    # el caso principal son 10 lecturas, la accion de S10 es el escenario adicional RA
    for e in base_elements:
        if e.get("type") == "text" and e.get("text") == "acción: 1 sola llamada":
            e["text"] = e["originalText"] = "si S10 fuera acción (escenario RA): 1 sola llamada"
            e["width"] = len(e["text"]) * e.get("fontSize", 16) * 0.56
    OUT.parent.mkdir(parents=True, exist_ok=True)
    doc = {
        "type": "excalidraw",
        "version": 2,
        "source": "https://excalidraw.com",
        "elements": base_elements + elements,
        "appState": {"gridSize": None, "viewBackgroundColor": "#ffffff"},
        "files": base.get("files", {}),
    }
    OUT.write_text(json.dumps(doc, ensure_ascii=False, indent=1), encoding="utf-8")
    print(f"escrito {OUT} ({len(base_elements)} del board original + {len(elements)} nuevos)")


if __name__ == "__main__":
    main()
