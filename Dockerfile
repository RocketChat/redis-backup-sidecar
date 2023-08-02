from golang:1.20-alpine3.17 as build

copy . /app

run cd /app && go build .

from alpine:3.17

copy --from=build /app/redis-sidecar /

cmd ["/redis-sidecar"]
