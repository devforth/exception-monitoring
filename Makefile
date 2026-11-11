IMAGE ?= devforth/exception-monitoring
VERSION ?= dev
MAX_IMAGE_MB ?= 50

.PHONY: test lint build run docker image-size

test:
	go test -race ./...

lint:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/exception-monitoring ./cmd/exception-monitoring

run:
	go run ./cmd/exception-monitoring

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

# Fails if the image is larger than MAX_IMAGE_MB.
image-size:
	@size=$$(docker image inspect $(IMAGE):$(VERSION) --format '{{.Size}}'); \
	mb=$$((size / 1024 / 1024)); \
	echo "image $(IMAGE):$(VERSION): $${mb} MB (limit $(MAX_IMAGE_MB) MB)"; \
	test $$mb -le $(MAX_IMAGE_MB)
