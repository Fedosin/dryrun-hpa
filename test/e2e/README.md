# End-to-end tests

The tests run against the current kube context and expect metrics-server and
the controller deployed with `config/e2e` (which enables `--compare-real-hpa`
and `--per-object-metrics`).

```sh
make kind-up            # kind cluster (podman provider if available) with metrics-server
make kind-load          # build the image and load it into the cluster
make deploy-e2e
make e2e
```

| Variable                   | Default                           | Meaning                                    |
|----------------------------|-----------------------------------|--------------------------------------------|
| `E2E_CONTROLLER_NAMESPACE` | `dryrun-hpa-system`               | Namespace of the controller                |
| `E2E_BUSY_IMAGE`           | `docker.io/library/busybox:1.37`  | Image for CPU burners and load generators  |
| `E2E_PHP_IMAGE`            | `registry.k8s.io/hpa-example`     | Image of the parity test workload          |
| `E2E_SKIP_PARITY`          |                                   | Skip the slow parity test                  |
| `E2E_KEEP_FAILED`          |                                   | Keep the namespaces of failed tests        |

The parity test runs the Kubernetes HPA walkthrough (php-apache under a fixed
request load) with a real HPA and a DryrunHPA that have the same spec and
target the same Deployment, and requires them to agree within one replica.
It takes about 15 minutes.
