docker compose -f stack/docker-compose.yml up -d

docker compose -f stack/docker-compose.yml up -d --force-recreate


docker compose -f stack/docker-compose.yml rm -f db-init

 ./stack/deploy.sh init-kms   