.PHONY: help dev dev-gallery build test check-js deploy deploy-fe clean

LISTEN_ADDR ?= :8080
BIN_DIR     ?= bin
BINARY      ?= $(BIN_DIR)/twitter-server

help:
	@echo "twitter-pic-go 常用开发与部署命令："
	@echo "  make dev          启动统一单端口服务（API + Gallery 同跑在 $(LISTEN_ADDR)，开启前端磁盘实时热更新）"
	@echo "  make build        编译统一单端口二进制文件 ($(BINARY) 与 ./server/server)"
	@echo "  make test         运行全量 Go 单元测试与前端 JS 语法检查"
	@echo "  make check-js     使用 node --check 校验 gallery/static/*.js 语法"
	@echo "  make deploy       完整构建并部署到生产服务器"
	@echo "  make deploy-fe    仅同步 gallery 前端模板与 CSS/JS 到服务器（免编译、零停机热更）"
	@echo "  make clean        清理编译产物与临时文件"

dev:
	LISTEN_ADDR=$(LISTEN_ADDR) GALLERY_DEV=1 GALLERY_FRONTEND_DIR=./gallery go run ./server

dev-gallery:
	GALLERY_ADDR=$(LISTEN_ADDR) GALLERY_DEV=1 GALLERY_FRONTEND_DIR=./gallery go run ./cmd/gallery

build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -o $(BINARY) ./server
	CGO_ENABLED=0 go build -o ./server/server ./server
	@echo "Build complete: $(BINARY) & ./server/server"

check-js:
	node --check gallery/static/home.js
	node --check gallery/static/app.js

test: check-js
	go test ./...

deploy:
	./build.sh

deploy-fe:
	./build.sh --frontend

clean:
	rm -rf $(BIN_DIR) ./server/server
