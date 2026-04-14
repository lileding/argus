FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /argus ./cmd/argus

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /argus /usr/local/bin/argus
WORKDIR /app
CMD ["argus", "--workspace", "/app/workspace", "--mode", "server"]
