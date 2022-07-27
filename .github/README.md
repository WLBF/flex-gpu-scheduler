# Flex GPU Scheduler Plugin

Kubernetes scheduler plugin for multi-container nvidia gpu share.

⚠This project is for code example purpose, if you want to use it in production, I am happy to provide support, feel
free to contact me.

Related project: [WLBF/flex-gpu-device-plugin](https://github.com/WLBF/flex-gpu-device-plugin)

## Overview

This scheduler plugin enable bin-pack scheduling decisions based on `nvidia.flex.com/gpu` and `nvidia.flex.com/memory` resource. 
Allow pods exclusive or share usage of nvidia gpus. Scheduled pod will be marked with `nividia.flex.com/index` annotation to denote its gpu index in running node.

## Install

Scheduler with plugin can be installed by helm chart. For development use `values.dev.yaml` instead of `values.pord.yaml`.

```
helm install flex-gpu-scheduler -f  ./manifests/flexgpu/values.prod.yaml ./manifests/flexgpu
```
