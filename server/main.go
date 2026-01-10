package main

import (
	"os"

	"github.com/Hana-ame/twitter-pic-go"
	"github.com/Hana-ame/twitter-pic-go/Tools/ginkit/middleware"
	"github.com/Hana-ame/twitter-pic-go/twimg"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	_ "github.com/joho/godotenv/autoload"
)

func main() {
	godotenv.Load(".env")

	go twimg.Run(os.Getenv("TWIMG_ADDR"))

	r := gin.Default()
	r.Use(middleware.CORS())

	api := r.Group("/api/twitter")

	twitter.AddToGroup(api)

	r.Run(os.Getenv("LISTEN_ADDR"))
}
