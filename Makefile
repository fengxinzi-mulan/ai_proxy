# 常用命令。Windows 上如果没有 make，直接用 build.bat 或 npm 命令即可。

BINARY := ai_proxy.exe
WEB_DIR := web

# PATH 上的 go 往往是旧版：g 用 ...\g\go 这个指针表示「当前版本」，而它可能停在 1.19；
# 机器级 GOROOT 也跟着指到那棵旧的源码树，于是 go build 会拿错标准库。这里固定用版本
# 目录里的高版本；该目录不存在（Linux/macOS）时退回 PATH 上的 go。可在命令行覆盖 GO_DIR。
GO_DIR ?= D:/Program Files/g/versions/1.25.4
GO := $(shell if [ -x "$(GO_DIR)/bin/go.exe" ]; then echo "$(GO_DIR)/bin/go.exe"; else command -v go; fi)
export GOROOT := $(shell if [ -x "$(GO_DIR)/bin/go.exe" ]; then echo "$(GO_DIR)"; else true; fi)

.PHONY: all web build run dev test clean tidy

all: build

## 只构建前端产物（会被 go:embed 打进二进制）
web:
	cd $(WEB_DIR) && npm install && npm run build

## 构建单文件可执行程序；依赖 web/dist 已存在
build:
	"$(GO)" build -o $(BINARY) .

## 完整构建：前端 + 后端
release: web build

## 本地运行（使用 data 目录存数据库）
run:
	"$(GO)" run . -data data

## 前端热更新开发模式；Go 服务需要另开一个终端跑 make run
dev:
	cd $(WEB_DIR) && npm run dev

test:
	"$(GO)" test ./...

tidy:
	"$(GO)" mod tidy
	cd $(WEB_DIR) && npm run typecheck

clean:
	rm -f $(BINARY)
	rm -rf $(WEB_DIR)/dist/* $(WEB_DIR)/node_modules .cache
