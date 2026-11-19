IMAGE ?= devforth/exception-monitoring
VERSION ?= dev
MAX_IMAGE_MB ?= 50

.PHONY: test lint fix build run backup docker image-size

test:
	go test -race ./...

# Needs golangci-lint v2 (config: .golangci.yml): https://golangci-lint.run/docs/welcome/install/
lint:
	golangci-lint run

# Fixes formatting, imports and blank lines; prints what is left to fix by hand.
fix:
	golangci-lint fmt
	golangci-lint run --fix

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/exception-monitoring ./cmd/exception-monitoring

# Loads .env and then .env.local (both optional, gitignored; see .env.example).
# Values from the files override the shell, so one-off variables like
# `LOG_LEVEL=debug make run` work only for keys the files don't set.
run:
	@set -a; [ -f .env ] && . ./.env; [ -f .env.local ] && . ./.env.local; set +a; \
	go run ./cmd/exception-monitoring

# A backup through the running server (or of the stopped database): DIR=/abs/path, by default DATA_DIR/backups/<time>.
backup:
	@set -a; [ -f .env ] && . ./.env; [ -f .env.local ] && . ./.env.local; set +a; \
	go run ./cmd/exception-monitoring backup $(DIR)

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

# Fails if the image is larger than MAX_IMAGE_MB.
image-size:
	@size=$$(docker image inspect $(IMAGE):$(VERSION) --format '{{.Size}}'); \
	mb=$$((size / 1024 / 1024)); \
	echo "image $(IMAGE):$(VERSION): $${mb} MB (limit $(MAX_IMAGE_MB) MB)"; \
	test $$mb -le $(MAX_IMAGE_MB)
