docker compose up -d mockserver payment-channel
docker ps --format '{{.Names}}' | grep mock