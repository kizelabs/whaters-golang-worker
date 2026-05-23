FROM golang:1.25-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=1 go build -o /out/whatsapp-service ./cmd/whatsapp-service

FROM debian:bookworm-slim
RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates sqlite3 \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=build /out/whatsapp-service /app/whatsapp-service
VOLUME ["/data"]
ENV DATA_DIR=/data
EXPOSE 8080
CMD ["/app/whatsapp-service"]
