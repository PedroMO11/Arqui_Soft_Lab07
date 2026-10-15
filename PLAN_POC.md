# Plan de implementación del PoC — Hedged Requests en Go

Este documento es la especificación para construir el PoC del Lab #10 ("A Tale of Tails"). Está pensado para entregárselo a un asistente de IA junto con el [README.md](README.md). **Sigue las decisiones de este plan; si algo no se puede cumplir, detente y pregunta antes de cambiar el diseño.**

---

## 1. Contexto mínimo

- Una API consulta **10 servicios en paralelo** y responde cuando **los 10 terminaron** (no se permiten respuestas parciales).
- Los servicios son **black boxes**: solo los llamamos. Toda la estrategia vive en **nuestra API**.
- De vez en cuando, cualquier servicio tarda mucho (1–1.5 s) y arrastra la latencia de toda la request (*tail latency*).
- Solución: un **Hedging Client** en la API con dos modos, comparados contra un baseline:

| Modo (`MODE`) | Comportamiento por servicio de **lectura** |
|---|---|
| `baseline` | 1 llamada. Se espera lo que tarde. |
| `hedged` (Modo A) | 1ª llamada. Si no respondió al llegar al **p95 de ese servicio**, se lanza una 2ª llamada al **mismo endpoint**. Gana la primera respuesta; la otra se cancela. |
| `double` (Modo B) | 2 llamadas al mismo endpoint en t = 0. Gana la primera; la otra se cancela. |

- **S10 es una acción** ("Crear solicitud"): en **todos** los modos se llama **una sola vez**, nunca se duplica.
- Supuestos de latencia de los servicios: normal 10–30 ms; con probabilidad **p = 2 %** la llamada es lenta (1000–1500 ms). Cada llamada sortea su latencia de forma independiente.

## 2. Reglas para la implementación

1. **Go 1.23, solo librería estándar** (`net/http`, `context`, `time`, `sync`, `sync/atomic`, `encoding/json`, `math/rand/v2`). Sin frameworks web.
2. **Todo corre en Docker** (Go y Locust no están instalados en la máquina). Nada debe depender de herramientas locales salvo Docker, Docker Compose y bash (Git Bash en Windows).
3. Código pequeño y legible: es un PoC, no un producto. Sin capas ni abstracciones que no se usen.
4. No agregar técnicas fuera del plan (caché, retries, circuit breakers, respuestas parciales, etc.).
5. Cada paso de la sección 5 termina con su verificación "**Hecho cuando**". No pasar al siguiente paso si la verificación falla.

## 3. Estructura del repositorio

```
poc/
├── mockservice/
│   ├── main.go            # black box simulada
│   ├── go.mod
│   └── Dockerfile
├── api/
│   ├── main.go            # servidor HTTP: /aggregate, /metrics, /metrics/reset, /health
│   ├── hedging.go         # Hedging Client (baseline | hedged | double)
│   ├── hedging_test.go    # tests unitarios del Hedging Client
│   ├── metrics.go         # contadores y latencias por servicio
│   ├── services.json      # configuración de los 10 servicios (url, action, p95_ms)
│   ├── go.mod
│   └── Dockerfile
├── loadtest/
│   └── locustfile.py
├── scripts/
│   └── run_experiment.sh  # corre baseline, hedged y double con la misma carga
├── results/               # salida de los experimentos (CSV + JSON)
└── docker-compose.yml
```

## 4. Especificación de componentes

### 4.1 `mockservice` — black box simulada

Un solo binario que se levanta 10 veces (`s1` … `s10`) con distinta configuración.

**Variables de entorno:**

| Variable | Default | Descripción |
|---|---|---|
| `SERVICE_NAME` | `svc` | Nombre en logs y respuestas |
| `PORT` | `8080` | Puerto HTTP |
| `NORMAL_MIN_MS` / `NORMAL_MAX_MS` | `10` / `30` | Rango de latencia normal (uniforme) |
| `SLOW_PROB` | `0.02` | Probabilidad de que una llamada sea lenta |
| `SLOW_MIN_MS` / `SLOW_MAX_MS` | `1000` / `1500` | Rango de latencia lenta (uniforme) |
| `MAX_CONCURRENCY` | `0` | Llamadas atendidas a la vez; `0` = ilimitado. Si se llena, las llamadas esperan en cola (esa espera cuenta como latencia). Simula la capacidad limitada del proveedor. |
| `IS_ACTION` | `false` | Solo informativo (se devuelve en la respuesta y en `/stats`) |

**Endpoints:**

- `GET /call` y `POST /call`: sortea la latencia, espera y responde `200` con
  `{"service":"s7","latency_ms":1234,"slow":true}`.
  - La espera debe hacerse con `select` sobre `time.After(latencia)` **y** `r.Context().Done()`. Si el contexto se cancela (el cliente cortó la conexión), se incrementa `cancelled` y se retorna sin responder.
- `GET /stats`: `{"service":"s7","received":N,"completed":N,"cancelled":N,"slow":N,"is_action":false}` (contadores `atomic`).
- `POST /stats/reset`: pone los contadores en 0.
- `GET /health`: `200 OK`.

### 4.2 `api` — agregador con Hedging Client

**Variables de entorno:**

| Variable | Default | Descripción |
|---|---|---|
| `MODE` | `baseline` | `baseline` \| `hedged` \| `double` |
| `SERVICES_FILE` | `services.json` | Ruta de la configuración de servicios |
| `HEDGE_BUDGET` | `0.10` | Solo `hedged`: máximo de 2ª llamadas como fracción de las 1ª llamadas a lecturas |
| `REQUEST_TIMEOUT_MS` | `5000` | Timeout total de `/aggregate` |
| `PORT` | `8080` | Puerto HTTP |

**`services.json`:**

```json
[
  {"name": "s1",  "url": "http://s1:8080/call",  "action": false, "p95_ms": 30},
  {"name": "s2",  "url": "http://s2:8080/call",  "action": false, "p95_ms": 30},
  ...
  {"name": "s10", "url": "http://s10:8080/call", "action": true,  "p95_ms": 30}
]
```

Los `p95_ms` iniciales (30) son provisionales; se reemplazan con los valores medidos en la calibración (paso 6).

**Cliente HTTP:** un único `http.Client` compartido, con `Transport` configurado para reutilizar conexiones (`MaxIdleConns: 1000`, `MaxIdleConnsPerHost: 200`, `IdleConnTimeout: 90s`). Esto evita que la propia API forme cola esperando conexiones. **Siempre** leer y cerrar el `Body` de las respuestas.

**Hedging Client (`hedging.go`)** — función central por servicio, conceptualmente:

```go
// Devuelve la primera respuesta exitosa. Cancela la(s) otra(s) llamada(s).
func (h *Hedger) Call(ctx context.Context, svc Service) (Result, error)
```

Comportamiento:

1. Crear `ctx, cancel := context.WithCancel(ctx)` y `defer cancel()`: al salir, se cancela la llamada perdedora.
2. Canal de resultados con buffer 2 (para que la goroutine perdedora nunca quede bloqueada).
3. Si `svc.Action` **o** `MODE == baseline` → 1 sola llamada y esperar su resultado.
4. `MODE == double` → lanzar 2 llamadas en t = 0; devolver la primera exitosa.
5. `MODE == hedged` → lanzar la 1ª; `select` entre el resultado y `time.After(p95)`:
   - si llega el resultado antes → devolverlo;
   - si vence el p95 y **hay presupuesto** → lanzar la 2ª llamada y devolver la primera exitosa de las dos;
   - si no hay presupuesto → seguir esperando la 1ª.
6. Si una llamada falla (error de red o status ≠ 2xx) y la otra sigue en curso, esperar a la otra. Si fallan todas → error.
7. Registrar en métricas: llamada primaria, hedge disparado, quién ganó (`primary` / `hedge`), hedge omitido por presupuesto, latencia final del servicio.

**Presupuesto (`hedged`):** permitir una 2ª llamada solo si `hedges_fired < HEDGE_BUDGET × primary_read_calls` (contadores desde el último reset).

**Endpoints de la API:**

- `GET /aggregate`: fan-out a los 10 servicios en paralelo (`sync.WaitGroup`), cada uno con `Hedger.Call`. Responde cuando terminan los 10:
  ```json
  {"mode":"hedged","total_ms":52,
   "services":[{"name":"s7","latency_ms":51,"hedged":true,"winner":"hedge"}, ...]}
  ```
  Si algún servicio falla → `502` con el detalle.
- `GET /metrics`: JSON con `mode`, `aggregate_requests`, `internal_calls_total`, `hedges_fired`, `hedges_won`, `hedges_skipped_budget` y, **por servicio**, `calls`, `p50_ms`, `p95_ms`, `p99_ms` de la latencia observada (guardar las últimas 10 000 latencias por servicio en un buffer circular protegido con `sync.Mutex`).
- `POST /metrics/reset`: reinicia contadores y latencias.
- `GET /health`: `200 OK`.

### 4.3 `docker-compose.yml`

- `s1` … `s10`: misma imagen de `mockservice`, `SERVICE_NAME` distinto; `s10` con `IS_ACTION=true`. **No** publicar sus puertos al host (solo red interna), excepto si se necesita para depurar.
- `api`: `MODE=${MODE:-baseline}`, publica `8080:8080`, monta `api/services.json`.
- `locust`: imagen `locustio/locust` con **versión fijada** (verificar con `docker pull` la última estable y fijarla, por ejemplo `locustio/locust:2.31.8`). Monta `loadtest/` y `results/`. Usar un `profile` (`loadtest`) para que no arranque con `docker compose up` normal.
- Dockerfiles multi-stage: build con `golang:1.23-alpine`, runtime con `alpine:3.20`. Binario estático (`CGO_ENABLED=0`).

### 4.4 `loadtest/locustfile.py`

```python
from locust import HttpUser, task, constant

class Cliente(HttpUser):
    wait_time = constant(0.1)

    @task
    def aggregate(self):
        self.client.get("/aggregate", name="/aggregate")
```

Ejecución (headless, misma carga para los 3 modos):

```bash
docker compose --profile loadtest run --rm locust \
  -f /mnt/loadtest/locustfile.py --headless \
  -u 50 -r 10 -t 3m --host http://api:8080 \
  --csv /mnt/results/<modo> --only-summary
```

### 4.5 `scripts/run_experiment.sh`

Para cada modo en `baseline hedged double`:

1. `MODE=<modo> docker compose up -d --build api` (los mocks quedan arriba).
2. Esperar `/health` de la API.
3. Warm-up: Locust 30 s (descartar resultados).
4. `POST /metrics/reset` en la API y `POST /stats/reset` en los 10 mocks (vía `docker compose exec` o un contenedor con `curl` en la red).
5. Corrida real: Locust 3 min → `results/<modo>_stats.csv`.
6. Guardar `GET /metrics` → `results/<modo>_metrics.json` y los `/stats` de los mocks → `results/<modo>_mocks.json`.

Parámetros (`USERS`, `DURATION`) configurables por variable de entorno con defaults `50` y `3m`.

## 5. Pasos de implementación (en orden)

| # | Paso | Hecho cuando |
|---|---|---|
| 1 | `mockservice` + Dockerfile | `docker compose up s1` y `curl` a `/call` 200 veces: ~2 % de respuestas con `slow:true`, el resto entre 10 y 30 ms. |
| 2 | `hedging.go` + `hedging_test.go` | `docker run --rm -v ${PWD}/poc/api:/src -w /src golang:1.23-alpine go test ./...` pasa con **todos** los tests de la sección 6.1. |
| 3 | `api` (`/aggregate`, `/metrics`, `/health`) + Dockerfile | Con los 10 mocks arriba, `curl localhost:8080/aggregate` devuelve los 10 servicios y `total_ms` coherente. |
| 4 | `docker-compose.yml` completo | `docker compose up -d --build` levanta 11 contenedores sanos. |
| 5 | `locustfile.py` + `run_experiment.sh` | Una corrida corta (`DURATION=30s`) genera los CSV y JSON en `results/`. |
| 6 | **Calibración del p95** | Corrida `baseline` completa; copiar el `p95_ms` por servicio de `results/baseline_metrics.json` a `services.json`. |
| 7 | Experimento final | Corridas `baseline`, `hedged` y `double` con la misma carga y los p95 calibrados. |
| 8 | Validaciones | Todos los checks de la sección 6.2 cumplen (o se explica por qué no). |
| 9 | README | Completar las secciones 2 y 3 (ver sección 7 de este plan). |

## 6. Validaciones

### 6.1 Tests unitarios del Hedging Client (con `httptest.Server` y latencias fijas)

1. **Primaria rápida (10 ms), p95 = 30 ms, `hedged`** → responde en < 30 ms y **no** dispara hedge.
2. **Primaria lenta (1 s), 2ª rápida (10 ms), p95 = 30 ms, `hedged`** → responde en < 100 ms, `winner = hedge`, y el servidor detecta la **cancelación** de la primaria.
3. **Servicio acción con primaria lenta, `hedged` y `double`** → el servidor recibe **exactamente 1** llamada.
4. **`double`** → el servidor recibe 2 llamadas casi simultáneas; se devuelve la más rápida.
5. **`baseline`** con primaria lenta → espera la lenta (~1 s) y hace 1 sola llamada.
6. **Presupuesto agotado** (`HEDGE_BUDGET = 0`) → no se dispara hedge aunque se pase el p95.
7. **Primaria con error 500 y 2ª exitosa** → devuelve la exitosa.

### 6.2 Checks sobre los resultados del experimento

| Check | Esperado |
|---|---|
| Llamadas extra en `hedged` | ≈ 5 % de las llamadas a lecturas (por definición del p95) y nunca > `HEDGE_BUDGET` |
| Llamadas extra en `double` | ≈ 100 % de las lecturas (≈ +90 % del total, porque S10 no se duplica) |
| S10 (`s10` `/stats.received`) | = requests a `/aggregate` en **todos** los modos |
| `cancelled` en los mocks | > 0 en `hedged` y `double`; ≈ 0 en `baseline` |
| CPU de la API (`docker stats`) | Holgada (< ~70 %). Si se satura, bajar `USERS`: la API no puede ser el cuello de botella |
| Requests ejecutadas | Del mismo orden en los 3 modos (misma carga) |

**Resultado esperado (no es un bug):** en el baseline ~18 % de las requests tiene algún servicio lento, así que p95 y p99 deberían estar en ~1–1.5 s. En `hedged` y `double`, el p95 debería bajar a decenas de ms. **El p99 puede seguir alto** porque S10 (acción) no se duplica y es lento el 2 % de las veces (~2.4 % de las requests). Hay que reportarlo tal cual: es parte del trade-off.

### 6.3 Experimentos opcionales (si hay tiempo)

- **Solo lecturas:** repetir `hedged` con `s10` como lectura (`action: false`) para mostrar el efecto sin la acción.
- **Capacidad limitada:** repetir los 3 modos con `MAX_CONCURRENCY=20` en los mocks y más usuarios (por ejemplo `USERS=150`) para ver si `double` empeora bajo carga por duplicar llamadas.

## 7. Qué completar en el README

**Sección 2 (PoC):** cómo levantarlo (`docker compose ...` y `scripts/run_experiment.sh`), qué simula cada componente, parámetros usados y la justificación del lenguaje:

> Elegimos Go porque el agregador es I/O-bound y altamente concurrente (10 a 20 llamadas por request). Las goroutines y `context` implementan el hedging (timer, 2ª llamada y cancelación) con la librería estándar, y al usar todos los cores sin un event loop de un solo hilo, la API no agrega cola propia que contamine la medición de la tail latency. Descartamos Rust por la curva de aprendizaje frente al plazo del PoC.

**Sección 3 (Resultados):** completar la tabla existente con los CSV de Locust (`50%`, `95%`, `99%`, `Request Count` de la fila `/aggregate`) y `internal_calls_total` de `/metrics`:

| Métrica | Baseline | Modo A (hedged p95) | Modo B (2 desde el inicio) |
|---|---|---|---|
| p50 | | | |
| p95 | | | |
| p99 | | | |
| Requests ejecutadas (cliente → API) | | | |
| Llamadas internas a servicios (costo) | | | |

Incluir también: parámetros de carga (usuarios, duración), los p95 calibrados por servicio y los resultados de los checks 6.2.
