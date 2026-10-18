package main

import (
	"os"
	"strings"

	"github.com/Hana-ame/twitter-pic-go/twimg"
	"github.com/joho/godotenv"
)

func main() {
	_ = godotenv.Load(".env")
	addr := strings.TrimSpace(os.Getenv("TWIMG_LISTEN"))
	if addr == "" {
		addr = strings.TrimSpace(os.Getenv("TWIMG_ADDR"))
	}
	if addr == "" {
		addr = "127.26.8.10:8080"
	}
	twimg.TwimgProxyV2(addr)
}
