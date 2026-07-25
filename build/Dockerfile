# Shared multi-stage Dockerfile for all standard Go services.
# Usage: docker build --build-arg SERVICE=dsp -f build/Dockerfile -t adtech-dsp .

FROM golang:1.25-alpine@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587 AS builder
ARG SERVICE
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /app ./cmd/${SERVICE}

FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
RUN apk add --no-cache ca-certificates && adduser -D -u 1000 app
COPY --from=builder /app /app
USER 1000:1000
ENTRYPOINT ["/app"]
