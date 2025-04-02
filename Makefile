start_server:
	go run ./cmd/server/main.go

start_client:
	go run ./cmd/client/main.go

start:
	make -j2 start_client start_server
