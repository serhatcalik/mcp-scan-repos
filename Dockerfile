FROM golang:1.26.10-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api
RUN go build -trimpath -ldflags="-s -w" -o /out/worker ./cmd/worker
RUN go build -trimpath -ldflags="-s -w" -o /out/mcp ./cmd/mcp

FROM alpine:3.24
RUN apk add --no-cache ca-certificates && addgroup -S app && adduser -S -G app app
USER app
WORKDIR /app
COPY --from=build /out/* /app/
EXPOSE 8080 8081
ENTRYPOINT ["/app/api"]
