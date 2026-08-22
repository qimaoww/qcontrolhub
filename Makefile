SHELL := /bin/sh

VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet fmt-check frontend-check installer-test web-image-test check init-env compose-config up dev-up down logs

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags '$(LDFLAGS)' -o bin/qcontrol-plane ./cmd/control-plane
	CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags '$(LDFLAGS)' -o bin/qagent ./cmd/agent

test:
	go test ./...

vet:
	go vet ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || { printf '%s\n' 'Go files need formatting; run gofmt -w on the listed files:'; gofmt -l .; exit 1; }

frontend-check:
	node frontend/module_smoke.mjs

installer-test:
	sh deploy/tests/inherit-existing-core.sh

web-image-test:
	docker build --target qcontrol-web --build-arg VERSION='$(VERSION)' .

check: fmt-check frontend-check installer-test vet test

init-env:
	@command -v openssl >/dev/null 2>&1 || { printf '%s\n' 'openssl is required'; exit 1; }
	@test ! -e .env || { printf '%s\n' '.env already exists; refusing to overwrite it'; exit 1; }
	@umask 077; \
	db_password="$$(openssl rand -hex 32)"; \
	admin_token="$$(openssl rand -hex 32)"; \
	webhook_secret="$$(openssl rand -hex 32)"; \
	config_key="$$(openssl rand -hex 32)"; \
	printf '%s\n' \
		'POSTGRES_DB=qcontrolhub' \
		'POSTGRES_USER=qcontrolhub' \
		"POSTGRES_PASSWORD=$$db_password" \
		'POSTGRES_PORT=5432' \
		"QCH_ADMIN_TOKEN=$$admin_token" \
		"QCH_WEBHOOK_SECRET=$$webhook_secret" \
		"QCH_CONFIG_ENCRYPTION_KEY=$$config_key" \
		'QCH_BEHIND_TLS_PROXY=true' \
		'QCH_ALLOW_INSECURE_HTTP=false' \
		'QCH_ALLOW_INSECURE_DATABASE=true' \
		'QCH_CORS_ORIGINS=https://qcontrolhub.example.com' \
		'QCH_CONTROL_PROXY_SUBNET=172.30.254.0/24' \
		'QCH_CONTROL_PROXY_GATEWAY=172.30.254.1' \
		'QCH_WEB_PROXY_ADDRESS=172.30.254.2' \
		'QCH_CONTROL_PLANE_PROXY_ADDRESS=172.30.254.3' \
		'QCH_TRUSTED_PROXY_CIDRS=172.30.254.2/32,172.30.254.1/32' \
		'QCH_BIND_ADDRESS=127.0.0.1' \
		'QCH_PORT=8080' \
		'QCH_IMAGE_TAG=latest' \
		'VERSION=$(VERSION)' > .env; \
	printf '%s\n' '.env created with mode 0600; store its secrets in a password manager.'

compose-config:
	docker compose config --quiet

up:
	docker compose pull
	docker compose up -d

dev-up:
	QCH_IMAGE_TAG=local QCH_BEHIND_TLS_PROXY=false QCH_ALLOW_INSECURE_HTTP=true QCH_ALLOW_INSECURE_DATABASE=true docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f qcontrol-web control-plane postgres
