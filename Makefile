VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
IMAGE   ?= ghcr.io/simplyzee/whatchanged
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: build test lint run dev image chart-lint clean

build: ## Build the binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/whatchanged ./cmd/whatchanged

test: ## Run tests with the race detector
	go test -race -count=1 ./...

lint: ## gofmt + go vet
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run gofmt -w ." && exit 1)
	go vet ./...

dev: build ## Run against your current kube context via kubectl proxy
	@echo "starting kubectl proxy on :8001"; kubectl proxy --port=8001 & \
	trap 'kill %1' EXIT; sleep 1; \
	WHATCHANGED_INGEST_TOKEN=dev ./bin/whatchanged server --api-server http://127.0.0.1:8001 --log-format text

image: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE):$(VERSION) .

chart-lint: ## Lint and render the Helm chart
	helm lint deploy/helm/whatchanged
	helm template wc deploy/helm/whatchanged > /dev/null

clean:
	rm -rf bin dist
