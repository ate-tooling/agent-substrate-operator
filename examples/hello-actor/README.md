# Hello actor

The smallest actor that shows an install works: one worker, and an actor
running busybox httpd. Nothing needs building.

It needs `kubectl ate` (`go install ./cmd/kubectl-ate` from an upstream
checkout), and, for the last step, the router allowlist that
[`../kind.yaml`](../kind.yaml) sets in `atenet.ingressAllowedSPIFFEIDs`.

```bash
kubectl apply -f examples/hello-actor/workerpool.yaml
kubectl ate create atespace smoke
kubectl ate create actor-template -f examples/hello-actor/actor-template.yaml
kubectl ate create actor hello -a smoke --template hello
kubectl ate resume actor hello -a smoke
kubectl ate get actor hello -a smoke   # ACTOR_STATE_RUNNING
```

Actors are reached through atenet-router over mTLS, not by pod IP. The proxy
is an allowlisted client and serves the actor on localhost:8000:

```bash
kubectl ate proxy actor hello -a smoke &
curl localhost:8000/   # hello from substrate
```

To clean up:

```bash
kubectl ate delete actor hello -a smoke
kubectl ate delete actor-template hello -a smoke
kubectl ate delete atespace smoke
kubectl delete namespace smoke
```
