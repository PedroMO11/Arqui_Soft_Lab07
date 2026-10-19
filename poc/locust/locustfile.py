"""Locust para el PoC de tail latency.

Cada usuario hace 1 request/s (constant_throughput), asi N usuarios = N req/s,
parecido a la carga open-loop de loadgen. El endpoint se elige con la variable
de entorno TAIL_ENDPOINT (aggregate o baseline).

    TAIL_ENDPOINT=aggregate locust -f locust/locustfile.py --headless -u 200 -r 200 \
        -t 40s --host http://127.0.0.1:8080 --csv results/locust_aggregate

Locust es closed-loop. Si una request se demora, el usuario no manda la
siguiente hasta que vuelva (coordinated omission), por lo tanto los numeros
oficiales salen de loadgen y Locust se usa como validacion cruzada.
"""

import os

from locust import FastHttpUser, constant_throughput, events, task

ENDPOINT = os.environ.get("TAIL_ENDPOINT", "aggregate")


class FanoutUser(FastHttpUser):
    wait_time = constant_throughput(1)

    @task
    def aggregate(self):
        with self.client.get(f"/{ENDPOINT}", name=f"/{ENDPOINT}", catch_response=True) as r:
            if r.status_code != 200:
                r.failure(f"status {r.status_code}")
                return
            body = r.json()
            if len(body.get("results", [])) != 10:
                r.failure("no convergieron las 10 respuestas")
                return
            r.success()


@events.quitting.add_listener
def _(environment, **_kw):
    s = environment.stats.total
    print(
        f"[{ENDPOINT}] requests={s.num_requests} fails={s.num_failures} "
        f"p50={s.get_response_time_percentile(0.5)} p95={s.get_response_time_percentile(0.95)} "
        f"p99={s.get_response_time_percentile(0.99)} ms"
    )
