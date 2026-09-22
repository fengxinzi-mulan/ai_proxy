# 常用命令。Windows 上如果没有 make，直接用 build.bat 或 npm 命令即可。

BINARY := ai_proxy.exe
WEB_DIR := web

.PHONY: all web build run dev test clean tidy

all: build

## 只构建前端产物（会被 go:embed 打进二进制）
web:
	cd $(WEB_DIR) && npm install && npm run build

## 构建单文件可执行程序；依赖 web/dist 已存在
build:
	go build -o $(BINARY) .

## 完整构建：前端 + 后端
release: web build

## 本地运行（使用 data 目录存数据库）
run:
	go run . -data data

## 前端热更新开发模式；Go 服务需要另开一个终端跑 make run
dev:
	cd $(WEB_DIR) && npm run dev

test:
	go test ./...

tidy:
	go mod tidy
	cd $(WEB_DIR) && npm run typecheck

clean:
	rm -f $(BINARY)
	rm -rf $(WEB_DIR)/dist/* $(WEB_DIR)/node_modules .cache
