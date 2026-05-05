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

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/amb-relay .

# Start a new stage from scratch
FROM alpine:latest

WORKDIR /root/

# Copy the binary from the builder stage
COPY --from=builder /app/amb-relay .

# Expose port 3334
EXPOSE 3334

# Command to run the executable
CMD ["./amb-relay"]
