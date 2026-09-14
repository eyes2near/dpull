PKG := dpull/internal
BIN := dpull
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
OUT := bin/$(BIN)
# 安装目录：默认 ~/.local/bin —— 已在 PATH 上且不需要 sudo。
# 想装到系统目录：make install PREFIX=/usr/local/bin（该目录需已存在且可写，否则要 sudo）
PREFIX ?= $(HOME)/.local/bin
PKGS := ./...

.PHONY: build install uninstall test race vet fmt fmt-check lint clean dist help

help: ## 显示可用目标
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n",$$1,$$2}'

build: ## 编译当前平台到 bin/dpull
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(OUT) ./cmd/dpull

install: ## 安装到 $(PREFIX)，默认 ~/.local/bin（已在 PATH 上）
	@mkdir -p $(DESTDIR)$(PREFIX)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(DESTDIR)$(PREFIX)/$(BIN) ./cmd/dpull
	@echo "已安装 -> $(DESTDIR)$(PREFIX)/$(BIN)"
	@case ":$$PATH:" in *":$(DESTDIR)$(PREFIX):"*) ;; \
	  *) echo "提示：$(DESTDIR)$(PREFIX) 不在当前 PATH 上，请加入 ~/.zshrc 或 ~/.profile："; \
	     echo "      export PATH=\"$(DESTDIR)$(PREFIX):\$$PATH\"" ;; esac
	@$(DESTDIR)$(PREFIX)/$(BIN) version

uninstall: ## 删除 $(DESTDIR)$(PREFIX)/$(BIN)
	rm -f $(DESTDIR)$(PREFIX)/$(BIN)
	@echo "已移除 $(DESTDIR)$(PREFIX)/$(BIN)"

test: ## 单元测试
	go test $(PKGS)

race: ## 带竞态检测的测试（下载器并发较多，建议常跑）
	go test -race -count=3 $(PKGS)

vet: ## go vet
	go vet $(PKGS)

fmt: ## 格式化
	gofmt -w cmd internal

fmt-check: ## 只检查格式，不改动文件（CI 用）
	@test -z "$$(gofmt -l cmd internal)" || { echo "gofmt 需要整理，请运行: make fmt"; gofmt -l cmd internal; exit 1; }
	@echo "gofmt 干净"

lint: fmt vet ## 格式化 + vet

clean: ## 清理产物
	rm -rf bin dist

dist: ## 交叉编译 4 个平台的静态二进制到 dist/
	@rm -rf dist && mkdir -p dist
	@for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "  -> dist/$(BIN)-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/$(BIN)-$$os-$$arch ./cmd/dpull; \
	done
