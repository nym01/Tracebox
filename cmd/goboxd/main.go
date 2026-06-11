package main

import (
	"log"
	"net/http"

	"github.com/nym01/goboxd/internal/api"
	"github.com/nym01/goboxd/internal/language"
)

func main() {
	if err := language.LoadRegistry("configs/languages.yaml"); err != nil {
		log.Fatalf("startup: %v", err)
	}
	api.InitReadyz()

	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	log.Println("listening on :8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatal(err)
	}
}
