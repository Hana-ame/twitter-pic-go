// Command gallery 提供仅启动 Gallery SSR 前端的独立调试入口。
// 注意：正式环境与完整本地联调请使用统一单端口入口 `go run ./server`（或 `go run ./cmd/server`），
// 统一入口会在同一地址同时挂载 API 与 Gallery（见 server/main.go）。
package main

import (
	"os"

	"github.com/Hana-ame/twitter-pic-go/gallery"
	"github.com/joho/godotenv"
	_ "github.com/joho/godotenv/autoload"
)

func main() {
	_ = godotenv.Load(".env")
	gallery.Run(os.Getenv("GALLERY_ADDR"))
}
