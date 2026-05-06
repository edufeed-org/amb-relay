FROM golang:1.25-alpine AS builder

WORKDIR /app

# git is needed for direct VCS fetches when GOPRIVATE bypasses the module proxy.
RUN apk add --no-cache git

# Copy go mod and sum files
COPY go.mod go.sum ./

# nostrlib is hosted on a private host; proxy.golang.org indexes it
# slowly (hours), so bypass the proxy for that module path.
ENV GOPRIVATE=git.edufeed.org

# Download dependencies (nostrlib resolved from git.edufeed.org)
RUN go mod download

# Copy source code
COPY . .

# Build the relay and the hydrate-bolt one-shot.
# hydrate-bolt is shipped in the image so operators can run it via
# `docker compose run --rm --entrypoint /root/hydrate-bolt amb-relay`
# to reconcile a Typesense-vs-BoltDB skew without rebuilding.
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/amb-relay . && \
    CGO_ENABLED=0 GOOS=linux go build -o /app/hydrate-bolt ./cmd/hydrate-bolt

# Start a new stage from scratch
FROM alpine:latest

WORKDIR /root/

# Copy the binaries from the builder stage
COPY --from=builder /app/amb-relay .
COPY --from=builder /app/hydrate-bolt .

# Expose port 3334
EXPOSE 3334

# Command to run the executable
CMD ["./amb-relay"]
