package main

import (
	"os"

	"github.com/Hana-ame/twitter-pic-go/gallery"
	"github.com/joho/godotenv"
	_ "github.com/joho/godotenv/autoload"
)

func main() {
	godotenv.Load(".env")
	gallery.Run(os.Getenv("GALLERY_ADDR"))
}
