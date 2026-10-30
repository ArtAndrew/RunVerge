# SPDX-License-Identifier: Elastic-2.0
FROM node:24.21.0-bookworm-slim@sha256:d6aa754f16b3197301076f047b5def2f02ea1dbbc2ca920407d46d7ec7f87b20 AS web-dependencies
WORKDIR /deps/web
COPY web/package.json web/pnpm-lock.yaml ./
RUN npm install --global pnpm@10.34.9 && pnpm install --frozen-lockfile --ignore-scripts

FROM golang:1.27.3-bookworm@sha256:df4bb0305771186a85d0fd3a40f5c5da9af97ac8053d391ffce2c070817a20c6 AS build
RUN apt-get update && apt-get install -y --no-install-recommends python3 ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=web-dependencies /usr/local/bin/node /usr/local/bin/node
WORKDIR /src/runverge
COPY . .
COPY --from=web-dependencies /deps/web/node_modules ./web/node_modules
ENV GOMODCACHE=/work/go-mod GOCACHE=/work/go-cache GOTOOLCHAIN=local GOENV=off GOWORK=off
RUN go mod download
RUN go test -mod=readonly ./cmd/healthcheck
RUN go test -mod=readonly ./cmd/platform -run '^Test(ContainerPreviewDoesNotRelaxDefaultOrIdentityListeners|PrepareModel)' -count=1
RUN go test -mod=readonly ./internal/modelgateway ./internal/modelruntime -count=1
ARG RUNVERGE_VERSION=0.14.0-alpha.3
RUN mkdir -m 0755 /out && python3 scripts/build_native_bundle.py --output /out/bundle --version "$RUNVERGE_VERSION" > /out/build-report.json
RUN hash="$(sha256sum /out/bundle/bundle.json | cut -d ' ' -f 1)" && /out/bundle/bin/platform bundle install --source /out/bundle --destination /out/installed --manifest-sha256 "$hash"
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -buildvcs=false -o /out/healthcheck ./cmd/healthcheck && mkdir -m 0755 /out/runtime-root && mkdir -m 0700 /out/runtime-root/state

# Verify on the target Linux system, with a fresh owned PostgreSQL cluster.
# The final image depends on this stage; a cross-build alone is insufficient.
FROM postgres:18.6-bookworm@sha256:5c4783a8cc3f0074912164f94dfa1be4e043438f0f707cf968d7af9a0d31a2c8 AS native-check
RUN apt-get update && apt-get install -y --no-install-recommends python3 && rm -rf /var/lib/apt/lists/*
COPY scripts /src/runverge/scripts
COPY --from=build --chown=postgres:postgres /out/bundle /opt/bundle
COPY --from=build --chown=postgres:postgres /out/installed /opt/installed
USER postgres
RUN python3 /src/runverge/scripts/verify_native_bundle.py --bundle /opt/bundle --manifest-sha256 "$(sha256sum /opt/bundle/bundle.json | cut -d ' ' -f 1)" --evidence /tmp/linux-native-smoke.json --team-pilot

FROM scratch AS evidence
COPY --from=native-check /tmp/linux-native-smoke.json /linux-native-smoke.json
COPY --from=build /out/build-report.json /build-report.json

FROM scratch AS runtime
COPY --from=native-check --chown=10001:10001 /opt/installed /app
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/healthcheck /usr/local/bin/runverge-healthcheck
# Copy state as a child directory; COPY otherwise creates its destination as 0755.
COPY --from=build --chown=10001:10001 /out/runtime-root/ /
USER 10001:10001
WORKDIR /app
ENV HOME=/tmp TZ=UTC
EXPOSE 8080
ENTRYPOINT ["/app/bin/platform"]
CMD ["serve", "--container-preview", "--listen", "0.0.0.0:8080"]
