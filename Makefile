BINARY := goboxd
CMD    := ./cmd/goboxd

.PHONY: build run test integration bench lint

build:
	go build -o $(BINARY) $(CMD)

run: build
	./$(BINARY)

test:
	go test ./...

integration:
	go test -tags integration ./...

bench:
	go test -bench=. -benchmem ./...

lint:
	go vet ./...
