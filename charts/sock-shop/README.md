# Sock Shop Helm Chart

Install the chart into the `sock-shop` namespace:

```bash
helm upgrade --install sock-shop ./helm/sock-shop --namespace sock-shop --create-namespace
```

Disable the synthetic load generator if needed:

```bash
helm upgrade --install sock-shop ./helm/sock-shop --namespace sock-shop --create-namespace --set userLoad.enabled=false
```

The default `user-load` workload uses a simple BusyBox loop to generate in-cluster HTTP traffic against `front-end`, which avoids the crashes from the legacy `weaveworksdemos/load-test` image.