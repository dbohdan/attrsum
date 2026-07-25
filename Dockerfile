# Build stage
# golang 1.25-alpine, 2026-02-28
FROM golang@sha256:f6751d823c26342f9506c03797d2527668d095b0a15f1862cddb4d927a7a4ced AS builder

RUN apk add --no-cache git make gcc musl-dev binutils-gold

# golangci-lint v2.10.1
RUN go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@5d1e709b7be35cb2025444e19de266b056b7b7ee
# goimports v0.42.0
RUN go install golang.org/x/tools/cmd/goimports@009367f5c17a8d4c45a961a3a509277190a9a6f0

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Run all checks - build fails if any check fails
RUN make check

# Build the binary
RUN make build

# Runtime stage
# alpine 3.21, 2026-02-28
FROM alpine@sha256:c3f8e73fdb79deaebaa2037150150191b9dcbfba68b4a46d70103204c53f4709

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /src/attrsum /app/attrsum

ENTRYPOINT ["/app/attrsum"]
