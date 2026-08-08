GO ?= go
GOFLAGS ?=

BINARY := build/cloudnet
CONFIG := configs/10-cloudnet.conf
INSTALL_BINARY := /opt/cni/bin/cloudnet
INSTALL_CONFIG := /etc/cni/net.d/10-cloudnet.conf

.PHONY: build test test-race vet install integration clean-test verify

build:
	mkdir -p build
	$(GO) build $(GOFLAGS) -trimpath -o $(BINARY) ./cmd/cloudnet

test:
	$(GO) test $(GOFLAGS) ./...

test-race:
	$(GO) test $(GOFLAGS) -race ./...

vet:
	$(GO) vet $(GOFLAGS) ./...

install: build
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "make install requires root; run: sudo make install" >&2; \
		exit 1; \
	fi
	install -d -m 0755 /opt/cni/bin /etc/cni/net.d
	install -m 0755 $(BINARY) $(INSTALL_BINARY)
	install -m 0644 $(CONFIG) $(INSTALL_CONFIG)

integration: build
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "make integration requires root; install first, then run: sudo make integration" >&2; \
		exit 1; \
	fi
	./hack/integration-v1.sh

clean-test:
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "make clean-test requires root; run: sudo make clean-test" >&2; \
		exit 1; \
	fi
	./hack/cleanup-v1.sh

verify:
	$(MAKE) test
	$(MAKE) test-race
	$(MAKE) vet
	$(MAKE) build
