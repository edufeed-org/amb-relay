FROM golang:1.25-alpine AS builder

WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./

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
