GO ?= go
GOFLAGS ?=

BINARY := build/cloudnet
CONFIG := configs/10-cloudnet.conf
INSTALL_BINARY := /opt/cni/bin/cloudnet
INSTALL_CONFIG := /etc/cni/net.d/10-cloudnet.conf

.PHONY: build test test-race vet check-built install check-installed integration clean-test verify

build:
	mkdir -p build
	$(GO) build $(GOFLAGS) -trimpath -o $(BINARY) ./cmd/cloudnet

test:
	$(GO) test $(GOFLAGS) ./...

test-race:
	$(GO) test $(GOFLAGS) -race ./...

vet:
	$(GO) vet $(GOFLAGS) ./...

check-built:
	@if [ ! -x "$(BINARY)" ]; then \
		echo "$(BINARY) not found; run: make build" >&2; \
		exit 1; \
	fi
	@if [ ! -f "$(CONFIG)" ]; then \
		echo "$(CONFIG) not found" >&2; \
		exit 1; \
	fi
	@if find cmd internal -type f -name '*.go' -newer "$(BINARY)" -print -quit | grep -q . \
		|| [ go.mod -nt "$(BINARY)" ] \
		|| [ go.sum -nt "$(BINARY)" ]; then \
		echo "$(BINARY) is older than Go sources or module metadata; run: make build" >&2; \
		exit 1; \
	fi

install: check-built
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "make install requires root; run: make build && sudo make install" >&2; \
		exit 1; \
	fi
	install -d -m 0755 /opt/cni/bin /etc/cni/net.d
	install -m 0755 $(BINARY) $(INSTALL_BINARY)
	install -m 0644 $(CONFIG) $(INSTALL_CONFIG)

check-installed:
	@if [ ! -x "$(INSTALL_BINARY)" ]; then \
		echo "$(INSTALL_BINARY) not found; run: sudo make install" >&2; \
		exit 1; \
	fi
	@if [ ! -f "$(INSTALL_CONFIG)" ]; then \
		echo "$(INSTALL_CONFIG) not found; run: sudo make install" >&2; \
		exit 1; \
	fi
	@cmp -s "$(BINARY)" "$(INSTALL_BINARY)" || { \
		echo "installed plugin differs from $(BINARY); run: make build && sudo make install" >&2; \
		exit 1; \
	}
	@cmp -s "$(CONFIG)" "$(INSTALL_CONFIG)" || { \
		echo "installed config differs from $(CONFIG); run: sudo make install" >&2; \
		exit 1; \
	}

integration: check-built check-installed
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "make integration requires root; run: sudo make integration" >&2; \
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
