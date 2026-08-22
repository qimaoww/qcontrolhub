# syntax=docker/dockerfile:1.7

FROM golang:1.25-alpine AS build-base

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

FROM build-base AS build-qcontrol-plane

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -buildvcs=false \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/qcontrol-plane \
    ./cmd/control-plane

FROM build-base AS build-qagent

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -buildvcs=false \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/qagent \
    ./cmd/agent

FROM alpine:3.22 AS runtime-base

ARG VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/qimaoww/qcontrolhub" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="GPL-3.0-only"

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S qcontrolhub \
    && adduser -S -D -H -G qcontrolhub qcontrolhub \
    && install -d -o qcontrolhub -g qcontrolhub /var/lib/qcontrolhub

USER qcontrolhub:qcontrolhub
WORKDIR /var/lib/qcontrolhub
STOPSIGNAL SIGTERM

FROM runtime-base AS qcontrol-plane

COPY --from=build-qcontrol-plane /out/qcontrol-plane /usr/local/bin/qcontrol-plane
COPY --from=build-qagent /out/qagent /usr/local/lib/qcontrolhub/qagent
COPY deploy/remote/install-agent.sh /usr/local/lib/qcontrolhub/install-agent.sh

ENV QCH_AGENT_BINARY_PATH=/usr/local/lib/qcontrolhub/qagent
ENV QCH_AGENT_INSTALLER_PATH=/usr/local/lib/qcontrolhub/install-agent.sh

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/qcontrol-plane"]

FROM runtime-base AS qagent

COPY --from=build-qagent /out/qagent /usr/local/bin/qagent

VOLUME ["/var/lib/qcontrolhub"]
ENTRYPOINT ["/usr/local/bin/qagent"]

FROM nginx:1.27-alpine AS qcontrol-web

ARG VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/qimaoww/qcontrolhub" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="GPL-3.0-only"

COPY frontend/index.html /usr/share/nginx/html/index.html
COPY frontend/app.js /usr/share/nginx/html/assets/app.js
COPY frontend/modules /usr/share/nginx/html/assets/modules
COPY frontend/app.css /usr/share/nginx/html/assets/app.css
COPY deploy/remote/install-agent.sh /usr/share/nginx/html/install-agent.sh
COPY deploy/bootstrap-core-services.sh /usr/share/nginx/html/install-assets/deploy/bootstrap-core-services.sh
COPY deploy/existing-core-mapping.sh /usr/share/nginx/html/install-assets/deploy/existing-core-mapping.sh
COPY deploy/systemd/qagent-core-journal.conf /usr/share/nginx/html/install-assets/deploy/systemd/qagent-core-journal.conf
COPY deploy/systemd/qagent-mihomo.service /usr/share/nginx/html/install-assets/deploy/systemd/qagent-mihomo.service
COPY deploy/systemd/qagent-xray.service /usr/share/nginx/html/install-assets/deploy/systemd/qagent-xray.service
COPY deploy/systemd/qagent-sing-box.service /usr/share/nginx/html/install-assets/deploy/systemd/qagent-sing-box.service
COPY deploy/systemd/qagent-shadowsocks-rust.service /usr/share/nginx/html/install-assets/deploy/systemd/qagent-shadowsocks-rust.service
COPY deploy/systemd/qagent.service /usr/share/nginx/html/install-assets/deploy/systemd/qagent.service
COPY examples/configs /usr/share/nginx/html/install-assets/examples/configs
COPY frontend/nginx.conf /etc/nginx/nginx.conf
RUN css_version="$(sha256sum /usr/share/nginx/html/assets/app.css | cut -c1-16)" \
    && js_content_version="$(find /usr/share/nginx/html/assets -type f -name '*.js' -print0 | sort -z | xargs -0 sha256sum | sha256sum | cut -c1-10)" \
    && js_version="${js_content_version}-$(printf '%s' "${VERSION}" | sha256sum | cut -c1-10)" \
    && sed -i -E "s#(from \"\\./modules/[^\"]+\\.js)\"#\\1?v=${js_version}\"#g" /usr/share/nginx/html/assets/app.js \
    && sed -i \
      -e "s/__QCH_CSS_VERSION__/${css_version}/g" \
      -e "s/__QCH_JS_VERSION__/${js_version}/g" \
      /usr/share/nginx/html/index.html

EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=3s --retries=6 CMD wget -q -O - http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["nginx", "-g", "daemon off;"]
