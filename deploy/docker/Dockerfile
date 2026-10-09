# syntax=docker/dockerfile:1
# The isshoni server image (docs/m1/06-deploy-and-ci.md §6.1). Built by goreleaser's dockers_v2 at a release and by
# deploy/docker/test/docker-build.sh in CI and for `task docker:smoke`. The build context holds the prebuilt static
# binary as linux/<arch>/isshoni, deploy/docker/rootfs/, LICENSE, NOTICE and THIRD_PARTY_NOTICES.
#
# No RUN steps: nothing is emulated for arm64, and the image holds only the static binary. No VOLUME either: an
# anonymous volume would hide a missing data volume, and the server refuses to start without one (exit 78).
#
# The digest is the multi-arch index of the :nonroot tag. Dependabot updates it (.github/dependabot.yml).
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
ARG TARGETPLATFORM
# The data directory and the admin socket's directory, both owned by nonroot (65532): a new named volume is then
# writable (Docker copies the ownership and the mode into an empty volume), and the admin socket has a home when no
# tmpfs is mounted there. Each directory is copied on its own, because one COPY of rootfs/ onto / would hand /var,
# /var/lib and /run to nonroot as well. The modes are those of a systemd install (StateDirectory, RuntimeDirectory).
COPY --chown=65532:65532 --chmod=0700 deploy/docker/rootfs/var/lib/isshoni/ /var/lib/isshoni/
COPY --chown=65532:65532 --chmod=0750 deploy/docker/rootfs/run/isshoni/ /run/isshoni/
COPY $TARGETPLATFORM/isshoni /usr/local/bin/isshoni
COPY LICENSE NOTICE THIRD_PARTY_NOTICES /usr/share/doc/isshoni/
# data_dir keeps its default /var/lib/isshoni, and the admin socket /run/isshoni/admin.sock. There is no config file
# in the image: the config comes from ISSHONI_* variables, and a mounted /etc/isshoni/isshoni.toml is optional.
ENV ISSHONI_IN_CONTAINER=1
EXPOSE 80/tcp 443/tcp 7882/udp 7882/tcp
USER 65532:65532
# Liveness, asked on the admin socket. The start period covers getting the first certificate.
HEALTHCHECK --interval=30s --timeout=5s --start-period=120s --retries=3 \
  CMD ["/usr/local/bin/isshoni", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/isshoni"]
CMD ["serve"]
