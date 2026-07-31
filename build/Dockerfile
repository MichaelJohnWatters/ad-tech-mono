# Shared multi-stage Dockerfile for all standard Go services.
# Usage: docker build --build-arg SERVICE=dsp -f build/Dockerfile -t adtech-dsp .

FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder
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
