package api

import (
	"log"
	"os"
	"testing"

	"github.com/nym01/goboxd/internal/language"
)

func TestMain(m *testing.M) {
	if err := language.LoadRegistry("../../configs/languages.yaml"); err != nil {
		log.Fatal(err)
	}
	os.Exit(m.Run())
}
