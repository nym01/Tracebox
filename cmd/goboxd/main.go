package main

import (
	"log"
	"net/http"

	"github.com/nym01/goboxd/internal/api"
)

func main() {
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	log.Println("listening on :8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatal(err)
	}
}
