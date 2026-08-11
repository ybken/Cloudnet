# 工具变量允许调用者覆盖，例如：make GO=/path/to/go GOFLAGS=-v。
GO ?= go
GOFLAGS ?=

# 工作区产物/配置与系统安装位置集中声明，避免各目标写死不同路径。
BINARY := build/cloudnet
CONFIG := configs/10-cloudnet.conf
INSTALL_BINARY := /opt/cni/bin/cloudnet
INSTALL_CONFIG := /etc/cni/net.d/10-cloudnet.conf

.PHONY: build test test-race vet check-built install check-installed integration clean-test verify

# 生成可复现性更好的无本机绝对路径二进制；本目标不需要 root。
build:
	mkdir -p build
	$(GO) build $(GOFLAGS) -trimpath -o $(BINARY) ./cmd/cloudnet

# 普通单元测试不创建真实 netns；特权端到端测试在 integration。
test:
	$(GO) test $(GOFLAGS) ./...

# race detector 重点覆盖并发 IPAM/锁测试。
test-race:
	$(GO) test $(GOFLAGS) -race ./...

# Go 静态检查，和 test 分开便于单独定位问题。
vet:
	$(GO) vet $(GOFLAGS) ./...

# 安装前不仅检查文件存在，还拒绝比源码/go.mod/go.sum 更旧的产物。
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

# 安装不隐式编译或下载依赖；先由普通用户 build，再显式 sudo 安装。
install: check-built
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "make install requires root; run: make build && sudo make install" >&2; \
		exit 1; \
	fi
	install -d -m 0755 /opt/cni/bin /etc/cni/net.d
	install -m 0755 $(BINARY) $(INSTALL_BINARY)
	install -m 0644 $(CONFIG) $(INSTALL_CONFIG)

# 集成测试前逐字节比较工作区与已安装文件，防止测试旧版本。
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

# 会创建真实 namespace/veth，因此要求 root 且依赖上述新鲜度检查。
integration: check-built check-installed
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "make integration requires root; run: sudo make integration" >&2; \
		exit 1; \
	fi
	./hack/integration-v1.sh

# 只调用带严格归属校验的测试清理脚本，并保留共享 Bridge。
clean-test:
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "make clean-test requires root; run: sudo make clean-test" >&2; \
		exit 1; \
	fi
	./hack/cleanup-v1.sh

# 本地完整验证顺序：单测、race、vet，最后生成通过检查的二进制。
verify:
	$(MAKE) test
	$(MAKE) test-race
	$(MAKE) vet
	$(MAKE) build
