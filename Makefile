run:
	go run ./cmd

deps:
	go mod tidy

up:
	docker compose up -d