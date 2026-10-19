# Lab #10 — A Tale of Tails: reducir la tail latency con Hedged Requests


**Integrantes:** Pedro Mori y Nicolás Castañeda

| Entregable | Estado |
|---|---|
| 1. Propuesta y diagrama | Este documento + [diagramas/arquitectura-hedged-v2.excalidraw](diagramas/arquitectura-hedged-v2.excalidraw) |
| 2. PoC | [poc/](poc/), sección 2 |
| 3. Resultados del experimento | [poc/results/RESULTS.md](poc/results/RESULTS.md), sección 3 |
| 4. Decisión (GO / GO WITH CHANGES / NO-GO) | **GO WITH CHANGES**, sección 4 |

---

## Contexto del problema

Una API tiene que consultar **10 servicios en paralelo** y juntar sus respuestas en una sola:

```
Cliente → API → 10 servicios → Respuesta
```

Normalmente los servicios responden en pocos milisegundos, pero de vez en cuando alguno tarda mucho más (por ejemplo 1–1.5 s). Como la API necesita las 10 respuestas, **la latencia que ve el usuario es la del servicio más lento**. Basta que **uno** se demore para que toda la request se demore. Este es el efecto *tail at scale* que describen Dean y Barroso (2013).

## Supuestos y restricciones

1. **Los servicios son _black boxes_** (por ejemplo RENIEC, SUNAT, préstamos). Solo los llamamos y recibimos una respuesta. No controlamos su código, sus réplicas ni su infraestructura, y no sabemos por qué se demoran. **Por eso toda la estrategia vive en nuestra API.**
2. **Los 10 servicios tienen que terminar** y converger en una sola respuesta. No se permiten respuestas parciales.
3. **Cualquiera de los 10 puede demorarse**, de forma ocasional y aleatoria.
4. En el caso principal los 10 servicios son **lecturas**. Como dato adicional se evalúa el caso en que uno **ejecuta una acción** (S10, "Crear solicitud"). Como es black box, no se sabe si llamarlo dos veces ejecuta la acción dos veces, por lo tanto una acción nunca se duplica. Ese caso es el escenario RA de la sección 3.
5. **La carga sobre los servicios puede duplicarse o triplicarse.** El profesor confirmó este supuesto, por lo tanto el límite de costo del experimento es carga ≤ 3x.

---

## 1. Propuesta de solución

### 1.1 Diagrama

**[diagramas/arquitectura-hedged-v2.excalidraw](diagramas/arquitectura-hedged-v2.excalidraw)** (se abre en [excalidraw.com](https://excalidraw.com) con *Open*). Arriba conserva el board original con las barras de tiempo y abajo agrega la vista de componentes, el trade-off, los resultados y la decisión. El board original sigue en [arquitectura-hedged.excalidraw](diagramas/arquitectura-hedged.excalidraw).

El diagrama compara el **baseline** con los **dos modos** de la propuesta, con una barra de tiempo por servicio:

- **Antes (baseline):** 9 servicios responden en 10–30 ms, pero S7 tarda 1.5 s y la API tiene que esperarlo → el usuario espera 1.5 s.
- **Modo A (hedging al p95):** cuando S7 pasa su p95 (~30 ms) se lanza una 2ª llamada que responde en ~20 ms y se cancela la 1ª → el usuario espera ~50 ms.
- **Modo B (2 llamadas desde t = 0):** cada lectura se llama 2 veces desde el inicio y se usa la más rápida del par. La llamada lenta de S7 nunca se espera → el usuario espera ~20 ms, pero se hace **el doble de llamadas siempre**. Si S10 fuera una acción (escenario RA), seguiría con 1 sola llamada.


### 1.2 ¿Qué cambiaríamos?

> **Propuesta final después del experimento:** **double+hedge-p95**, que combina los dos modos de abajo. Cada lectura sale con 2 copias en t = 0 (Modo B) y, si ninguna respondió al p95 observado del servicio, se manda una 3ª copia (Modo A). Con carga x2.05 baja el p99 de 1.47 s a 41.5 ms y el p99.9 de 1.50 s a 64.4 ms. La comparación contra el baseline y los dos modos está en la sección 3.

No tocamos los servicios. Cambiamos **cómo los llama nuestra API**: agregamos un **Hedging Client** entre el fan-out y los servicios.

El Hedging Client tiene **dos modos**, que comparamos en el experimento:

| | Baseline | Modo A · hedging al p95 | Modo B · 2 llamadas desde t = 0 |
|---|---|---|---|
| Llamadas por servicio de lectura | 1 | 1, o 2 si la 1ª no respondió al llegar a su p95 | 2 siempre |
| ¿Qué pasa si un servicio se demora? | Se espera, aunque tarde 1.5 s | Al llegar al p95 (~30 ms) sale la 2ª llamada al mismo endpoint → responde en ~50 ms | No se espera: la otra llamada ya estaba en curso → responde en ~20 ms |
| Acciones no idempotentes (escenario RA) | 1 llamada | 1 llamada (**nunca se duplican**) | 1 llamada (**nunca se duplican**) |
| Llamadas extra | 0 | ~5 % | ~100 % (el doble en las 10 lecturas, ~90 % si S10 fuera acción) |

**Algoritmo por servicio, Modo A:**

1. Lanzar la 1ª llamada.
2. Si responde antes de su p95 → listo.
3. Si no, y el servicio es lectura → lanzar la 2ª llamada al mismo endpoint.
4. Usar la primera respuesta que llegue y cancelar la otra.

**Algoritmo por servicio, Modo B**:

1. Si el servicio es lectura, lanzar 2 llamadas al mismo endpoint en t = 0. Si es acción, lanzar 1.
2. Usar la primera respuesta que llegue y cancelar la otra.

En los dos modos, la API responde cuando terminan los 10 servicios.

**Detalles internos del Hedging Client**:

- **p95 por servicio (Modo A):** cada servicio tiene su propio umbral. Al inicio se toma del baseline; como mejora, se recalcula con una ventana móvil de las últimas respuestas.
- **Hedge budget (Modo A, ≤ 10 % de llamadas extra):** si un proveedor se degrada por completo, todas sus llamadas pasarían el p95 y duplicaríamos la carga justo en el peor momento. El presupuesto corta las 2ª llamadas cuando se supera el límite. Es la misma idea que un rate limiter, pero aplicada a nuestras propias copias.
- **Guardia de acciones (ambos modos):** los servicios marcados como acción **nunca** se duplican, porque no se sabe si son idempotentes. En el escenario RA ese servicio es S10.

### 1.3 ¿Por qué debería mejorar la tail latency?

**a) El fan-out amplifica la cola.** Si cada servicio es lento con probabilidad *p*, la probabilidad de que la request completa sea lenta es:

$$P(\text{request lenta}) = 1 - (1 - p)^{10}$$

Con p = 2 %, **el 18.3 % de las requests** sale lenta, aunque cada servicio por separado sea rápido el 98 % de las veces. Por eso el p95 y el p99 del baseline caen en ~1–1.5 s.

**b) Que las dos llamadas sean lentas es muy improbable.** La lentitud de una black box suele venir de algo **puntual**. No viene de la request en sí. Una 2ª llamada al mismo endpoint tiene entonces una chance independiente de ser rápida, y el servicio solo sale lento si **ambas** lo son (≈ p²).

Estimación teórica con p = 2 %:

| Escenario | P(servicio lento) | P(request lenta) |
|---|---|---|
| Baseline | 2 % | **18.3 %** |
| Modo A o B, 10 lecturas (caso principal) | 0.04 % | **0.4 %** |
| Modo A o B, 9 lecturas + 1 acción (escenario RA) | 0.04 % (lecturas) / 2 % (acción) | **2.4 %** |

Los dos modos tienen la **misma probabilidad** de quedar lentos, porque en ambos el servicio solo se demora si sus 2 llamadas son lentas. La diferencia está en **cuánto tarda** el caso en que la 1ª llamada se demora.

**c) El peor caso típico queda acotado.** Cuando la 1ª llamada se demora y la 2ª es normal:

- **Modo A:** el servicio responde en **p95 + una respuesta normal** (≈ 30 + 20 = **50 ms**), no en 1.5 s.
- **Modo B:** la 2ª llamada salió en t = 0, así que responde en **~20 ms**, sin esperar el p95.

**d) Hay evidencia real.** En un benchmark de Google (BigTable, 1 000 claves en 100 servidores), mandar un hedge después de 10 ms bajó el p99.9 **de 1 800 ms a 74 ms con solo 2 % más requests** (Dean y Barroso, 2013).

### 1.4 ¿Qué trade-off introduce?

*Trade-off = beneficio / costo.*

| | Beneficio | Costo |
|---|---|---|
| **Modo A (hedged al p95)** | Baja mucho el p95 y el p99 de las lecturas sin tocar los servicios. Funciona sin importar la causa de la lentitud. | **~5 % más llamadas** a los servicios de lectura (por definición, ~5 % de las llamadas pasa su p95). Más complejidad en la API: timers, cancelación, p95 por servicio, presupuesto. |
| **Modo B (2 desde el inicio)** | Ahorra la espera del p95 (~30 ms) en las llamadas que se habrían demorado. Mejora un poco el p50. | **~+100 % de llamadas** (los 10 servicios duplicados siempre). Más costo si el proveedor cobra por consulta, se llega antes a sus rate limits, y bajo carga puede **empeorar** la cola por saturar al proveedor. |

**Limitaciones que asumimos:**

- **Si un proveedor está degradado por completo**, las dos llamadas son lentas y ninguna técnica del lado del cliente lo arregla. El hedge budget al menos evita empeorarlo.
- **Costo para terceros:** cada copia es una llamada real a un servicio externo, con posible costo y rate limits.

**Hipótesis a validar en el experimento:** el Modo A logra un p99 parecido al del Modo B con una fracción mínima de las llamadas extra (~5 % vs ~100 %).

---

## 2. PoC

El código está en [poc/](poc/). Usa Go 1.22+ con solo librería estándar, y Python con Locust y matplotlib para la validación y las gráficas.

### 2.1 Componentes

| Componente | Archivo | Qué simula o hace |
|---|---|---|
| Servicios black box | [poc/cmd/blackbox](poc/cmd/blackbox/main.go) | 10 servicios S1 a S10 en los puertos 9001 a 9010. Con el perfil `spec`, cada llamada tarda 10 a 30 ms y el 2% tarda 1 a 1.5 s, de forma independiente. El escenario `episode` degrada S7 y S8 por completo durante ~1.5 s cada ~20 s. Cada servicio corta el trabajo si el cliente cancela y expone `/stats` con llamadas recibidas, completadas y canceladas. |
| API agregadora | [poc/cmd/api](poc/cmd/api/main.go) | `GET /aggregate` hace fan-out a los 10 servicios y responde cuando convergen las 10 respuestas. La estrategia se configura por flags. `GET /baseline` queda siempre disponible para comparar. |
| Hedging Client | [poc/internal/fanout](poc/internal/fanout/fanout.go) | Por servicio lanza `InitialCopies` en t = 0 y, si no hubo respuesta al vencer el delay, otra copia hasta `MaxCopies`. Gana la primera respuesta y el resto se cancela con `context`. El delay es el pXX observado del servicio en una ventana de 1 000 respuestas ([tracker.go](poc/internal/fanout/tracker.go)) o un valor fijo. Un token bucket ([budget.go](poc/internal/fanout/budget.go)) limita las copias extra. Los servicios marcados como acción nunca se duplican. |
| Logs transaccionales | [poc/cmd/api](poc/cmd/api/main.go) | Logger asíncrono a CSV, una fila por request con su latencia. Si el buffer se llena descarta la línea, por lo tanto el logging nunca bloquea la request. |
| Generador de carga | [poc/cmd/loadgen](poc/cmd/loadgen/main.go) | Carga open-loop a tasa fija. Mide la latencia desde el instante en que la request debía salir, lo que evita el coordinated omission de los generadores closed-loop. Es la fuente oficial de los percentiles. |
| Locust | [poc/locust/locustfile.py](poc/locust/locustfile.py) | Validación cruzada de los percentiles con una herramienta estándar. Marca como fallo cualquier respuesta que no traiga las 10 partes. |
| Orquestador y reporte | [poc/scripts](poc/scripts/) | `run_experiments.py` corre el experimento con procesos limpios por corrida. `report.py` genera [RESULTS.md](poc/results/RESULTS.md) y las gráficas. |

Las estrategias del experimento salen de tres parámetros del mismo Hedging Client:

| Estrategia | Copias en t = 0 | Máx. copias | Siguiente copia | Budget |
|---|---:|---:|---|---|
| baseline | 1 | 1 | nunca | - |
| Modo A (hedged-p95) | 1 | 2 | al p95 observado | +10% |
| Modo B (double) | 2 | 2 | nunca | - |
| Propuesta (double+hedge-p95) | 2 | 3 | al p95 observado | carga ≤ 3x |

Go encaja con el problema porque la API es I/O-bound y muy concurrente (10 a 30 llamadas por request). Las goroutines, `context` y `time.Timer` implementan el hedge, la cancelación y el timer con la librería estándar. Además, al usar todos los cores sin un event loop de un solo hilo, la API no agrega una cola propia que contamine la medición de la tail latency.

### 2.2 Cómo correrlo

Con Go y Python instalados:

```bash
cd poc
go test ./...                              # tests del Hedging Client
python scripts/run_experiments.py          # experimento completo (~32 min), genera results/RESULTS.md
python scripts/run_experiments.py --quick  # versión corta para probar (~5 min)
```

Solo con Docker:

```bash
cd poc
docker compose up -d --build
API_FLAGS="-initial 1 -copies 2 -hedge-pct 95 -budget 0.1" docker compose up -d api
LABEL=hedged docker compose run --rm loadgen     # resumen en results/docker_hedged.json
```

### 2.3 Diferencias con [PLAN_POC.md](PLAN_POC.md)

- Los 10 servicios corren en un solo proceso con 10 puertos en lugar de 10 contenedores. El comportamiento por servicio es el mismo y la medición no paga el overhead de red de Docker en Windows.
- Los percentiles oficiales salen de un generador open-loop, y Locust queda como validación cruzada, porque Locust es closed-loop y subestima la cola.
- Además de los 2 modos se evalúa double+hedge-p95, que los combina, con la restricción de carga ≤ 3x confirmada por el profesor.
- El p95 de cada servicio se recalcula en línea con una ventana móvil en lugar de calibrarse a mano.

## 3. Resultados del experimento

El detalle completo de los 3 escenarios está en [poc/results/RESULTS.md](poc/results/RESULTS.md).

### 3.1 Método

- Carga open-loop de 150 req/s, 40 s medidos por corrida más 10 s de warm-up, por lo tanto 6 000 requests por corrida.
- 3 repeticiones por configuración con semillas distintas. Cada métrica es la mediana de las 3 y el p99 también se reporta con su rango [min-max].
- Procesos limpios en cada corrida (blackbox y API se reinician), con la misma semilla para todas las estrategias de una repetición.
- Restricción de costo, carga ≤ 3x sobre los servicios.
- 36 corridas en total (4 estrategias, 3 escenarios, 3 repeticiones), 0 errores.

Escenarios:

| Escenario | Qué simula | Para qué sirve |
|---|---|---|
| R | Lentitud por request, 2% de las llamadas tarda 1 a 1.5 s, los 10 servicios son lectura | Caso principal del enunciado |
| RA | Igual que R, pero S10 es acción y nunca se duplica | Dato adicional, costo de no poder duplicar una acción |
| E | Episodios, S7 y S8 se degradan por completo ~1.5 s cada ~20 s | Límite del hedging ante lentitud correlacionada |

### 3.2 Tabla del challenge (escenario R)

| Métrica | Baseline | Modo A (hedged p95, budget 10%) | Modo B (double) | **Propuesta (double+hedge-p95)** |
|---|---:|---:|---:|---:|
| p50 | 29.9 ms | 30.1 ms (+1%) | 26.8 ms (−11%) | **26.8 ms (−10%)** |
| p95 | 1.37 s | 57.1 ms (−96%) | 30.7 ms (−98%) | **30.9 ms (−98%)** |
| p99 | 1.47 s | 66.0 ms (−96%) | 32.7 ms (−98%) | **41.5 ms (−97%)** |
| p99.9 | 1.50 s | 1.29 s (−14%) | 1.26 s (−16%) | **64.4 ms (−96%)** |
| Requests ejecutadas (cliente → API) | 18 000 | 18 000 | 18 000 | 18 000 |
| Llamadas internas a servicios (costo) | 180 000 | 191 978 | 360 000 | 369 011 |
| Carga sobre los servicios | x1.00 | x1.07 | x2.00 | x2.05 |
| Requests > 100 ms | 18.27% | 0.45% | 0.43% | **0.00%** |

El piso físico de una request es ~30 ms, porque la API espera al más lento de 10 servicios que tardan 10 a 30 ms cada uno. double+hedge-p95 deja toda la distribución entre ese piso y 85 ms, mientras que el baseline deja al 18% de los usuarios por encima de 1 s.

![CCDF escenario R](poc/results/ccdf_R.png)

### 3.3 Lectura de los resultados

![Trade-off p99 vs carga](poc/results/tradeoff_R.png)

- **Las copias desde t = 0 son las que más bajan la cola.** Modo B y la propuesta no esperan nada antes de mandar la copia, por lo tanto su p95 y p99 quedan en ~31 a 41 ms, pegados al piso de ~30 ms.
- **El Modo A tiene un piso de ~55 ms en el p95.** Es el delay (~29 ms, el p95 aprendido de una latencia uniforme de 10 a 30 ms) más la respuesta de la copia. A cambio, solo agrega 7% de llamadas.
- **Con 2 copias queda una cola residual.** Modo A y Modo B bajan el p99, pero su p99.9 sigue en ~1.3 s, porque el 0.04% de las veces las 2 copias son lentas y con 10 servicios eso afecta al ~0.4% de las requests. La 3ra copia de la propuesta elimina ese escalón con 2.5% más de llamadas que el Modo B.
- **El hedge rescata justo las llamadas lentas.** En el Modo A gana la copia en ~1 200 llamadas por corrida, que coincide con el 2% lento de 6 000 requests × 10 servicios. Las copias que no ganan son el costo del mecanismo.

### 3.4 Escenarios RA y E

| Escenario | Métrica | Baseline | Modo A (hedged-p95) | Modo B (double) | Propuesta (double+hedge-p95) |
|---|---|---:|---:|---:|---:|
| RA, S10 acción | p95 | 1.38 s | 57.5 ms | 31.2 ms | 31.1 ms |
| RA, S10 acción | p99 | 1.48 s | 1.23 s | 1.26 s | 1.24 s |
| E, episodios | p95 | 1.36 s | 1.27 s | 1.25 s | 1.19 s |
| E, episodios | p99 | 1.47 s | 1.41 s | 1.38 s | 1.33 s |

- **RA.** S10 es lento el 2% de las veces y no se puede duplicar. Ese 2% supera el 1% del p99, por lo tanto el p99 queda en ~1.25 s para cualquier estrategia, mientras que el p95 baja igual que en R.
- **E.** Durante un episodio todas las copias del servicio degradado son lentas, así que el hedging solo recorta ~10%. Ninguna estrategia empeora la latencia ni genera errores.

![CCDF escenario RA](poc/results/ccdf_RA.png)
![Timeline escenario E](poc/results/timeline_E.png)

### 3.5 Validación con Locust y límites del experimento

- **Locust confirma el baseline.** Con la misma configuración mide p50 31 ms, p95 1.40 s y p99 1.50 s, frente a 30 ms, 1.37 s y 1.47 s del generador open-loop. La diferencia es menor al 3%.
- **Con double+hedge, Locust no es confiable en esta máquina.** Reporta p50 190 ms. Corriendo Locust y el generador al mismo tiempo contra la misma API, Locust mide p50 160 ms y el generador 27 ms. La diferencia viene de Locust: es Python en un solo core y compite por CPU con la API y los 10 servicios. Por eso los percentiles oficiales salen del generador en Go.
- **Todo corre en una sola laptop con Windows.** El ruido de procesos en background genera outliers aislados entre repeticiones, que la mediana de 3 repeticiones absorbe. Una validación en producción debe separar el generador de carga, la API y los servicios en máquinas distintas.

## 4. Decisión

**GO WITH CHANGES.** Se implementa **double+hedge-p95** en los 10 servicios de lectura.

La evidencia justifica la arquitectura. En el escenario R, el p99 baja de 1.47 s a 41.5 ms y el p99.9 de 1.50 s a 64.4 ms, con carga x2.05, dentro del límite de 3x aceptado. Ninguna request pasó de 100 ms, frente al 18% del baseline. double+hedge-p95 se elige sobre el Modo B, que tiene un p99 9 ms menor, porque el Modo B deja el p99.9 en 1.26 s y la propuesta cubre ese caso con 2.5% más de llamadas.

El GO queda condicionado a tres cambios que el PoC hizo visibles:

1. **Servicios que ejecutan acciones.** Una acción no idempotente no se puede duplicar, por lo tanto su lentitud no se recorta. En el escenario RA, con S10 como acción, el p99 queda en ~1.24 s y el p95 en 31 ms. Para incluir una acción en el hedging se solicita al proveedor una idempotency key, y mientras no exista el SLA de p99 de esa request se acepta en ~1.3 s.
2. **Degradación total de un proveedor.** El hedging no la resuelve (escenario E, p99 1.47 → 1.33 s). La API monitorea la tasa de copias por servicio para detectarla, y el budget mantiene la carga en ≤ 3x mientras dura.
3. **Costo de las copias.** Cada copia es una llamada real a un tercero (+105% de llamadas). Si un proveedor cobra por consulta o tiene rate limit bajo, ese servicio usa el Modo A, con carga x1.07 y p99 66 ms.

---

## Referencias

- Dean, J. y Barroso, L. A. (2013). *The Tail at Scale*. Communications of the ACM, 56(2), 74–80. ([tail-at-scale.pdf](tail-at-scale.pdf))
