# Lab #10 — A Tale of Tails: reducir la tail latency con Hedged Requests


**Integrantes:** Pedro Mori y Nicolás Castañeda

| Entregable | Estado |
|---|---|
| 1. Propuesta y diagrama | ✅ Este documento + [diagramas/arquitectura-hedged.excalidraw](diagramas/arquitectura-hedged.excalidraw) |
| 2. PoC | ⏳ Pendiente |
| 3. Resultados del experimento | ⏳ Pendiente |
| 4. Decisión (GO / GO WITH CHANGES / NO-GO) | ⏳ Pendiente |

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
4. Algunos servicios son **lecturas** y otros **ejecutan una acción**. Como son black boxes, no sabemos si llamar dos veces a una acción la ejecuta dos veces.

---

## 1. Propuesta de solución

### 1.1 Diagrama

📐 **[diagramas/arquitectura-hedged.excalidraw](diagramas/arquitectura-hedged.excalidraw)** (se abre en [excalidraw.com](https://excalidraw.com) → *Open*).

El diagrama compara el **baseline** con los **dos modos** de la propuesta, con una barra de tiempo por servicio:

- **Antes (baseline):** 9 servicios responden en 10–30 ms, pero S7 tarda 1.5 s y la API tiene que esperarlo → el usuario espera 1.5 s.
- **Modo A (hedging al p95):** cuando S7 pasa su p95 (~30 ms) se lanza una 2ª llamada que responde en ~20 ms y se cancela la 1ª → el usuario espera ~50 ms.
- **Modo B (2 llamadas desde t = 0):** cada lectura se llama 2 veces desde el inicio y se usa la más rápida del par. La llamada lenta de S7 nunca se espera → el usuario espera ~20 ms, pero se hace **el doble de llamadas siempre**. La acción (S10) sigue con 1 sola llamada.


### 1.2 ¿Qué cambiaríamos?

No tocamos los servicios. Cambiamos **cómo los llama nuestra API**: agregamos un **Hedging Client** entre el fan-out y los servicios.

El Hedging Client tiene **dos modos**, que comparamos en el experimento:

| | Baseline | Modo A · hedging al p95 | Modo B · 2 llamadas desde t = 0 |
|---|---|---|---|
| Llamadas por servicio de lectura | 1 | 1, o 2 si la 1ª no respondió al llegar a su p95 | 2 siempre |
| ¿Qué pasa si un servicio se demora? | Se espera, aunque tarde 1.5 s | Al llegar al p95 (~30 ms) sale la 2ª llamada al mismo endpoint → responde en ~50 ms | No se espera: la otra llamada ya estaba en curso → responde en ~20 ms |
| Acciones (S10) | 1 llamada | 1 llamada (**nunca se duplican**) | 1 llamada (**nunca se duplican**) |
| Llamadas extra | 0 | ~5 % | ~90 % (el doble en las 9 lecturas) |

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
- **Guardia de acciones (ambos modos):** los servicios marcados como acción (S10, "Crear solicitud") **nunca** se duplican, porque no sabemos si son idempotentes.

### 1.3 ¿Por qué debería mejorar la tail latency?

**a) El fan-out amplifica la cola.** Si cada servicio es lento con probabilidad *p*, la probabilidad de que la request completa sea lenta es:

$$P(\text{request lenta}) = 1 - (1 - p)^{10}$$

Con p = 2 %, **el 18.3 % de las requests** sale lenta, aunque cada servicio por separado sea rápido el 98 % de las veces. Por eso el p95 y el p99 del baseline caen en ~1–1.5 s.

**b) Que las dos llamadas sean lentas es muy improbable.** La lentitud de una black box suele venir de algo **puntual**. No viene de la request en sí. Una 2ª llamada al mismo endpoint tiene entonces una chance independiente de ser rápida, y el servicio solo sale lento si **ambas** lo son (≈ p²).

Estimación teórica con p = 2 %:

| Escenario | P(servicio lento) | P(request lenta) |
|---|---|---|
| Baseline | 2 % | **18.3 %** |
| Modo A, si los 10 fueran lecturas | 0.04 % | **0.4 %** |
| Modo A o B, 9 lecturas + 1 acción (nuestro caso) | 0.04 % (lecturas) / 2 % (acción) | **2.4 %** |

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
| **Modo B (2 desde el inicio)** | Ahorra la espera del p95 (~30 ms) en las llamadas que se habrían demorado. Mejora un poco el p50. | **~+90 % de llamadas** (9 de 10 servicios duplicados siempre). Más costo si el proveedor cobra por consulta, se llega antes a sus rate limits, y bajo carga puede **empeorar** la cola por saturar al proveedor. |

**Limitaciones que asumimos:**

- **Si un proveedor está degradado por completo**, las dos llamadas son lentas y ninguna técnica del lado del cliente lo arregla. El hedge budget al menos evita empeorarlo.
- **Costo para terceros:** cada copia es una llamada real a un servicio externo, con posible costo y rate limits.

**Hipótesis a validar en el experimento:** el Modo A logra un p99 parecido al del Modo B con una fracción mínima de las llamadas extra (~5 % vs ~90 %).

---

## 2. PoC

⏳ _Pendiente._

## 3. Resultados del experimento

⏳ _Pendiente._ Tabla a completar:

| Métrica | Baseline | Modo A (hedged p95) | Modo B (2 desde el inicio) |
|---|---|---|---|
| p50 | | | |
| p95 | | | |
| p99 | | | |
| Requests ejecutadas (cliente → API) | | | |
| Llamadas internas a servicios (costo) | | | |

## 4. Decisión

⏳ _Pendiente_ (GO / GO WITH CHANGES / NO-GO).

---

## Referencias

- Dean, J. y Barroso, L. A. (2013). *The Tail at Scale*. Communications of the ACM, 56(2), 74–80. ([tail-at-scale.pdf](tail-at-scale.pdf))
