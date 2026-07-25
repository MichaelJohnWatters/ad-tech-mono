# Shared multi-stage Dockerfile for all standard Go services.
# Usage: docker build --build-arg SERVICE=dsp -f build/Dockerfile -t adtech-dsp .

FROM golang:1.25-alpine AS builder
ARG SERVICE
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /app ./cmd/${SERVICE}

FROM alpine:3.20
RUN apk add --no-cache ca-certificates && adduser -D -u 1000 app
COPY --from=builder /app /app
USER 1000:1000
ENTRYPOINT ["/app"]
