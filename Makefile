start_server:
	go run ./cmd/server/main.go

start_client:
	go run ./cmd/client/main.go

start:
	make -j2 start_client start_server

build:
	go build -o files/client ./cmd/client/main.go
	go build -o files/server ./cmd/server/main.go
