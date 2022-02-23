FROM golang:latest AS build

WORKDIR /app/

COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags "-s -w" main.go

FROM debian:unstable-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app/

COPY --from=build /app/main /app/instances-api

EXPOSE 3000
CMD ./instances-api
