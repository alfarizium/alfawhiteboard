# Dockerfile for Railway deployment
FROM golang:1.21-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o whiteboard .

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/

COPY --from=builder /app/whiteboard .

# Railway automatically sets the PORT environment variable
CMD ["./whiteboard"]
