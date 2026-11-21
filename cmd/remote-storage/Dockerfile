# Copyright (c) 2024 The Jaeger Authors.
# SPDX-License-Identifier: Apache-2.0

ARG base_image
ARG debug_image

ARG SVC=remote-storage

FROM $base_image AS release
ARG TARGETARCH
ARG USER_UID=10001
ARG USER_GID=10001
COPY remote-storage-linux-$TARGETARCH /go/bin/remote-storage-linux
EXPOSE 16686/tcp
ENTRYPOINT ["/go/bin/remote-storage-linux"]
USER ${USER_UID}:${USER_GID}

FROM $debug_image AS debug
ARG TARGETARCH=amd64
ARG USER_UID=10001
ARG USER_GID=10001
COPY remote-storage-debug-linux-$TARGETARCH /go/bin/remote-storage-linux
EXPOSE 16686/tcp
# Delve does not authenticate clients, so it listens on loopback only. Attach from inside
# the container as the image user, e.g. `kubectl exec -it <pod> -- /go/bin/dlv connect 127.0.0.1:12345`.
ENTRYPOINT ["/go/bin/dlv", "exec", "/go/bin/remote-storage-linux", "--headless", "--listen=127.0.0.1:12345", "--api-version=2", "--accept-multiclient", "--log", "--"]
USER ${USER_UID}:${USER_GID}
