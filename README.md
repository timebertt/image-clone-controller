# image-clone-controller

## Background

### Goal

We'd like to be safe against the risk of public container images disappearing from the registry while we use them, breaking our deployments.

### Problem

We have a Kubernetes cluster on which we can run applications. These applications will often use publicly available container images, like official images of popular programs, e.g. Jenkins, PostgreSQL, and so on. Since the images reside in repositories over which we have no control, it is possible that the owner of the repo deletes the image while our pods are configured to use it.
In the case of a subsequent node rotation, the locally cached copies of the images would be deleted and Kubernetes would be unable to re-download them in order to re-provision the applications.

### Idea

Have a controller which watches the applications and mirrors the images to our own registry repository and reconfigures the applications to use these copies.

## Demo

[![asciicast](https://asciinema.org/a/509182.svg)](https://asciinema.org/a/509182)

[Play at asciinema.org](https://asciinema.org/a/509182)

## Usage

```bash
make kind-up # create local cluster including a local registry
make deploy # deploy the controller to kind
```

The registry and controller should be up and running:
```bash
$ k -n registry get deploy,svc
NAME                       READY   UP-TO-DATE   AVAILABLE   AGE
deployment.apps/registry   1/1     1            1           5m

NAME               TYPE       CLUSTER-IP   EXTERNAL-IP   PORT(S)          AGE
service/registry   NodePort   10.96.0.11   <none>        5001:30501/TCP   5m

$ k -n image-clone-system get deploy
NAME                     READY   UP-TO-DATE   AVAILABLE   AGE
image-clone-controller   1/1     1            1           4m
```

Now, you're ready to deploy your workload!

```bash
k create deployment nginx --image nginx
k create deployment grafana --image grafana/grafana:main
k create deployment speedtest-exporter --image ghcr.io/timebertt/speedtest-exporter:v0.1.0
```

`image-clone-controller` will start copying your images to the local backup registry and update the `Deployment` objects to reference the copied image:
```bash
$ k get po -o=custom-columns="NAME:.metadata.name,IMAGE:.spec.containers[0].image"
NAME                                  IMAGE
grafana-6f844c97f6-9d9dn              10.96.0.11:5001/index_docker_io/grafana/grafana:main
nginx-79cb9d6457-vrmpv                10.96.0.11:5001/index_docker_io/library/nginx:latest
speedtest-exporter-568df77fcd-mrftd   10.96.0.11:5001/ghcr_io/timebertt/speedtest-exporter:v0.1.0

$ k get po
NAME                                  READY   STATUS    RESTARTS   AGE
grafana-6f844c97f6-9d9dn              1/1     Running   0          3m
nginx-79cb9d6457-vrmpv                1/1     Running   0          3m
speedtest-exporter-568df77fcd-mrftd   1/1     Running   0          3m
```

The backup registry can be specified via the `--backup-registry` flag.
Images are rewritten and copied to the backup registry using the following scheme:
```text
# docker library images
nginx                                        -> <dstRegistry>/index_docker_io/library/nginx:latest
# tag is kept
nginx:1.23                                   -> <dstRegistry>/index_docker_io/library/nginx:1.23
# digest is rewritten to tag
nginx@sha256:33cef...                        -> <dstRegistry>/index_docker_io/library/nginx:sha256_33cef...
# non-library images from Docker Hub
grafana/grafana:main                         -> <dstRegistry>/index_docker_io/grafana/grafana:main
# other registries
ghcr.io/timebertt/speedtest-exporter:v0.1.0  -> <dstRegistry>/ghcr_io/timebertt/speedtest-exporter:v0.1.0
```

## Development

The controller is scaffolded with [kubebuilder](https://book.kubebuilder.io/) and implemented using [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime).
However, for the sake of simplicity, the default kubebuilder manifest structure has been trimmed down significantly.


You can easily develop this controller using [skaffold](https://skaffold.dev):

```bash
# create local cluster including a local registry
make kind-up

# use skaffold to build a fresh image and deploy to kind
make up # one-time build and deployment
make dev # dev loop with re-build an deployment on trigger
```
