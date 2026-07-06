# agentmon deploy — Loki + Grafana via Tilt

Brings up the agentmon backend on a Kubernetes cluster: Loki (event store)
and Grafana (dashboard + alerting), everything provisioned from the files
in `k8s/` — no click-ops.

## Bring-up

    cd deploy
    kubectl config use-context <your-lab-context>
    tilt up

Tilt port-forwards Loki to localhost:3100 and Grafana to localhost:3000
(anonymous admin — this is a private-network lab deployment; put real auth
in front of it before exposing it to anything).

For laptops to ship events, they need a route to Loki port 3100 — a
NodePort/Ingress/LoadBalancer on the lab, or Tailscale. Then per machine:

    # ~/.config/agentmon/config.toml
    [loki]
    url = "http://<lab-host>:3100"

    agentmon watch          # spools + drains every 10s
    agentmon drain --once   # manual kick / catch-up

## Alerts

`k8s/grafana/provisioning/alerting/alerting.yaml` ships one rule (idle
sessions → ntfy webhook). **Edit the ntfy topic** (`agentmon-CHANGEME`)
before relying on it, then subscribe to that topic in the ntfy app.

## Notes

- Loki accepts old timestamps (`reject_old_samples: false`) so
  `agentmon watch --backfill` can seed history.
- Grafana state is ephemeral (container layer — it dies on any pod
  restart); everything that matters is provisioned from this directory.
  `tilt down` removes it all; Loki data survives on its PVC.
- After editing provisioning ConfigMaps, restart the pod (`tilt trigger
  grafana` or delete the pod) — generated ConfigMaps don't auto-restart
  deployments.
- Loki is configured without retention enforcement (no compactor); add a
  retention_period + compactor stanza when volume warrants.
