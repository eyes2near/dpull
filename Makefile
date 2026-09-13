PKG := dpull/internal
BIN := dpull
OUT := bin/$(BIN)
PREFIX ?= /usr/local/bin
PKGS := ./...

.PHONY: build install test race vet fmt lint clean dist help

help: ## 显示可用目标
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n",$$1,$$2}'

build: ## 编译当前平台到 bin/dpull
	@mkdir -p bin
	go build -trimpath -ldflags "-s -w" -o $(OUT) ./cmd/dpull

install: ## 安装到 /usr/local/bin/dpull（可用 PREFIX 覆盖）
	go build -trimpath -ldflags "-s -w" -o $(DESTDIR)$(PREFIX)/$(BIN) ./cmd/dpull

test: ## 单元测试
	go test $(PKGS)

race: ## 带竞态检测的测试（下载器并发较多，建议常跑）
	go test -race -count=3 $(PKGS)

vet: ## go vet
	go vet $(PKGS)

fmt: ## 格式化
	gofmt -w cmd internal

lint: fmt vet ## 格式化 + vet

clean: ## 清理产物
	rm -rf bin dist

dist: ## 交叉编译 4 个平台的静态二进制到 dist/
	@rm -rf dist && mkdir -p dist
	@for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do \
		os=$${t%/*}; arch=$${t#*/}; \
		echo "  -> dist/$(BIN)-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w" \
			-o dist/$(BIN)-$$os-$$arch ./cmd/dpull; \
	done
