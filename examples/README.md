# Examples

## echo: two backends behind one Ingress

`examples/echo/` runs two copies of a small HTTP echo server behind one
Ingress served by sluice. The echo server (`mendhak/http-https-echo`) answers
every request with a JSON document describing what it received: method, path,
headers, and the `hostname` of the pod that answered. That makes the proxy's
work visible with nothing more than curl.

What the demo shows:

- Host and path routing. `Host: demo.localtest.me` with path `/a` reaches
  Service `echo-a`, `/b` reaches `echo-b`, any other host or path is a 404.
- Forwarded headers. The echo prints `x-forwarded-for`, `x-forwarded-host`,
  `x-forwarded-proto` and `x-request-id`, and its `host` is still
  `demo.localtest.me`: the backend sees the original Host.
- Header rules with placeholders. The annotation `sluice.dev/request-headers`
  adds `X-Sluice-Route: {k8s.route}` and `X-Sluice-Upstream: {upstream.address}`
  to the request sent upstream; `sluice.dev/response-headers` adds
  `X-Served-By: sluice` to the response.
- Round-robin over the ready endpoints of a Service, 503 when there are none,
  404 when the Ingress is gone, all without restarting sluice.
- The Prometheus metrics on the admin port.

### Files

| File | Contents |
|---|---|
| `echo/00-namespace.yaml` | Namespace `demo` |
| `echo/echo-a.yaml` | Deployment `echo-a` (1 replica) and ClusterIP Service `echo-a` on port 80 |
| `echo/echo-b.yaml` | The same for `echo-b` |
| `echo/ingress.yaml` | Ingress `demo`, class `sluice`, host `demo.localtest.me`, paths `/a` and `/b` |

`hack/demo.sh` (`make demo`) expects exactly these names: namespace `demo`,
Deployments `echo-a` and `echo-b`, Ingress `demo`, and sluice itself as
Deployment `sluice` in namespace `sluice-system`.

### Prerequisites

sluice deployed in the local kind cluster (docs/design/devx.md section 5):

```
colima start --cpu 4 --memory 8 --disk 60 --vm-type vz --mount-type virtiofs
make kind-up docker-build kind-load deploy
kubectl -n sluice-system rollout status deployment/sluice
```

### Apply

`kubectl apply -f` on a directory applies its files in name order, so the
namespace is created before the objects inside it:

```
kubectl apply -f examples/echo/
kubectl -n demo rollout status deployment/echo-a
kubectl -n demo rollout status deployment/echo-b
kubectl -n demo get ingress demo
```

No restart of sluice is involved. Its informers see the new Ingress, Services
and EndpointSlices, the controller rebuilds the routing table and publishes it
atomically. `kubectl -n sluice-system logs deployment/sluice` shows a
`config applied` line with the new version and route count.

### Requests

`deploy/kind/cluster.yaml` maps `127.0.0.1:8080` on the host to the NodePort
of the sluice Service, and the Host header selects the Ingress rule. These are
the requests `hack/demo.sh` makes:

```
curl -sS -H 'Host: demo.localtest.me' http://127.0.0.1:8080/a
curl -sS -H 'Host: demo.localtest.me' http://127.0.0.1:8080/b
curl -sS -o /dev/null -w '%{http_code}\n' -H 'Host: unknown.example.invalid' http://127.0.0.1:8080/
```

The first two print the echo JSON. Look at its `headers` object: `host` is
the original host, `x-forwarded-for` is the client address as seen by sluice,
`x-forwarded-host` and `x-forwarded-proto` describe the inbound connection,
`x-request-id` is the id sluice assigned (or forwarded, if the client sent
one), and `x-sluice-route` and `x-sluice-upstream` are the two header rules
from the Ingress annotation after placeholder evaluation. The third request
prints `404`: no Ingress rule matches that host.

The response header rule is visible with the headers included:

```
curl -sS -i -H 'Host: demo.localtest.me' http://127.0.0.1:8080/a | grep -i '^x-served-by'
```

`*.localtest.me` resolves to `127.0.0.1` in public DNS, so
`curl http://demo.localtest.me:8080/a` also works when the machine can
resolve it. The Host header form does not depend on DNS.

### Observe round-robin

Scale one backend to three pods, wait until they are ready, then read the
`hostname` field of a few consecutive responses:

```
kubectl -n demo scale deployment/echo-b --replicas=3
kubectl -n demo rollout status deployment/echo-b
for i in 1 2 3 4 5 6; do
  curl -sS -H 'Host: demo.localtest.me' http://127.0.0.1:8080/b | grep -o '"hostname": *"[^"]*"'
done
```

The three pod names rotate in a fixed order. sluice builds the endpoint list
from the Service's EndpointSlices, keeps only the endpoints marked `ready`
(which is why the pods must pass their readiness probe before they appear),
sorts them by address, and a round-robin picker walks that list. The counter
lives inside the routing table and starts again at the first address each
time a new table is published, so right after the scale-up the sequence starts
from the beginning.

Back to one replica:

```
kubectl -n demo scale deployment/echo-b --replicas=1
```

### Observe a 503 (route exists, no endpoint)

```
kubectl -n demo scale deployment/echo-b --replicas=0
curl -sS -o /dev/null -w '%{http_code}\n' -H 'Host: demo.localtest.me' http://127.0.0.1:8080/b
```

Within a second or so the answer is `503`. The endpoint is marked not ready
as soon as its pod starts terminating, the controller publishes a table where
the `/b` route still exists but has zero endpoints, and the proxy answers 503
for a route it cannot send anywhere. `/a` keeps working. Scale back:

```
kubectl -n demo scale deployment/echo-b --replicas=1
kubectl -n demo rollout status deployment/echo-b
```

### Observe a 404 (no route)

```
kubectl -n demo delete ingress demo
curl -sS -o /dev/null -w '%{http_code}\n' -H 'Host: demo.localtest.me' http://127.0.0.1:8080/a
kubectl apply -f examples/echo/ingress.yaml
```

The answer is `404` until the Ingress is re-applied: no rule matches, so the
request never reaches a backend. The difference with the previous section is
the whole point of the two status codes: 404 means "no route", 503 means "a
route, but nothing to send to".

### Read the metrics

The admin port is not exposed by the Service. Reach it with a port-forward,
as `hack/demo.sh` does:

```
kubectl -n sluice-system port-forward deployment/sluice 9090:9090 &
curl -sS http://127.0.0.1:9090/readyz
curl -sS http://127.0.0.1:9090/metrics | grep '^sluice_'
kill %1
```

What to look for:

- `sluice_http_requests_total{route,code}` and
  `sluice_http_request_duration_seconds{route}`: one series per route and
  status. The `route` label is the route key, for example `demo/demo#r0p0`
  for Ingress `demo` in namespace `demo`, rule 0, path 0 (`/a`); `#r0p1` is
  `/b`. Labels never carry the host, the path or any header value.
- `sluice_upstream_errors_total{route,kind}`: dial failures and timeouts, by
  route.
- `sluice_config_version`, `sluice_config_applied_total`,
  `sluice_config_rejected_total`, `sluice_ingresses_skipped_total` and
  `sluice_routes`: the control plane. The version grows by one on every table
  swap (every scale, every Ingress change above), `sluice_routes` is the
  number of routes in the current table (2 with the demo Ingress, 0 after
  deleting it).
- `sluice_ready`: 1 once the informers have synced and the first table has
  been published.

### What a bad Ingress does

The header rules are validated before anything is published. Put an unknown
placeholder in the annotation:

```
kubectl -n demo annotate ingress demo --overwrite 'sluice.dev/request-headers=X-Bad: {nope}'
curl -sS -o /dev/null -w '%{http_code}\n' -H 'Host: demo.localtest.me' http://127.0.0.1:8080/a
kubectl -n sluice-system logs deployment/sluice | grep -i skipped
```

The Ingress is skipped with a warning that names the annotation and the
variable (never the value), `sluice_ingresses_skipped_total` goes up, and its
routes disappear (404). Every other Ingress in the cluster is unaffected: one
typo never freezes updates for everyone else. Re-applying the file restores
the annotation and the routes:

```
kubectl apply -f examples/echo/ingress.yaml
```

### Clean up

```
kubectl delete -f examples/echo/
```

Deleting the namespace removes the Deployments, Services and the Ingress.
`make undeploy` removes sluice itself.
