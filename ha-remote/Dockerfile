FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY agent.go ./
RUN CGO_ENABLED=0 go build -o /bin/agent ./agent.go

FROM alpine:3.20
COPY --from=builder /bin/agent /usr/local/bin/agent
COPY --chmod=755 run.sh /run.sh
ENTRYPOINT ["/run.sh"]
