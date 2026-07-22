# openinspect-operator

![Release](https://img.shields.io/github/v/release/dirien/openinspect-operator?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square)

A Helm chart to distribute the project openinspect-operator

The chart version and appVersion are stamped from the git tag when the chart
is packaged and pushed; the version in the committed Chart.yaml is a
placeholder.

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| certmanager.enable | bool | `false` |  |
| controllerManager.container.args[0] | string | `"--leader-elect"` |  |
| controllerManager.container.args[1] | string | `"--metrics-bind-address=:8443"` |  |
| controllerManager.container.args[2] | string | `"--health-probe-bind-address=:8081"` |  |
| controllerManager.container.image.repository | string | `"ghcr.io/dirien/openinspect-operator"` | Operator image; the release publishes ghcr.io/dirien/openinspect-operator. |
| controllerManager.container.image.tag | string | `""` | Image tag. Empty follows the chart appVersion as v<appVersion>, which matches the release tagging — a released chart installs its matching images with no overrides. |
| controllerManager.container.imagePullPolicy | string | `"IfNotPresent"` |  |
| controllerManager.container.livenessProbe.httpGet.path | string | `"/healthz"` |  |
| controllerManager.container.livenessProbe.httpGet.port | int | `8081` |  |
| controllerManager.container.livenessProbe.initialDelaySeconds | int | `15` |  |
| controllerManager.container.livenessProbe.periodSeconds | int | `20` |  |
| controllerManager.container.modalDeployImage | string | `""` | Image run by modal-deploy Jobs (pinned Modal CLI + pinned upstream app source). Empty selects the release-matched ghcr.io/dirien/openinspect-modal-deploy image. |
| controllerManager.container.readinessProbe.httpGet.path | string | `"/readyz"` |  |
| controllerManager.container.readinessProbe.httpGet.port | int | `8081` |  |
| controllerManager.container.readinessProbe.initialDelaySeconds | int | `5` |  |
| controllerManager.container.readinessProbe.periodSeconds | int | `10` |  |
| controllerManager.container.resources.limits.cpu | string | `"500m"` |  |
| controllerManager.container.resources.limits.memory | string | `"128Mi"` |  |
| controllerManager.container.resources.requests.cpu | string | `"10m"` |  |
| controllerManager.container.resources.requests.memory | string | `"64Mi"` |  |
| controllerManager.container.runtimeBuilderImage | string | `""` | Image run by runtime-build Jobs (needs /runtime-builder from this repository). Empty follows the manager image, which is almost always right since the operator image ships both binaries. |
| controllerManager.container.securityContext.allowPrivilegeEscalation | bool | `false` |  |
| controllerManager.container.securityContext.capabilities.drop[0] | string | `"ALL"` |  |
| controllerManager.container.webImageDaytona | string | `""` | Release web image for provider daytona (spec.web.image empty). Empty selects the release-matched ghcr.io/dirien/openinspect-web image. |
| controllerManager.container.webImageModal | string | `""` | Release web image for provider modal (spec.web.image empty). Empty selects the release-matched ghcr.io/dirien/openinspect-web image. |
| controllerManager.replicas | int | `1` |  |
| controllerManager.securityContext.runAsNonRoot | bool | `true` |  |
| controllerManager.securityContext.seccompProfile.type | string | `"RuntimeDefault"` |  |
| controllerManager.serviceAccountName | string | `"openinspect-operator-controller-manager"` |  |
| controllerManager.terminationGracePeriodSeconds | int | `10` |  |
| crd.enable | bool | `true` |  |
| crd.keep | bool | `true` |  |
| metrics.enable | bool | `true` |  |
| networkPolicy.enable | bool | `false` |  |
| prometheus.enable | bool | `false` |  |
| rbac.enable | bool | `true` |  |
