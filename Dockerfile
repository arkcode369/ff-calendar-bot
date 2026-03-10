# Stage 1: Build
FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod .
COPY main.go .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o ffbot main.go

# Stage 2: Run (scratch = ~0MB base, minimal attack surface)
FROM alpine:latest

# tzdata for timezone support
RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app
COPY --from=builder /app/ffbot .

ENV TZ=Asia/Jakarta

ENTRYPOINT ["./ffbot"]
