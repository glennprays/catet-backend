
# Stage 1: Build the Go application
FROM golang:1.26 AS builder

# Set the current working directory inside the container
WORKDIR /app

# Copy the go.mod and go.sum files to download dependencies
COPY go.mod go.sum ./

# Download all the dependencies
RUN go mod download

# Copy the source code
COPY . .

# Build metadata, passed in by the CI workflow / local `docker build`.
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

# Build the Go application with version info baked in via -ldflags.
# -s -w strips DWARF/symbol tables for a smaller production binary.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-s -w \
      -X github.com/glennprays/catet-backend/pkg/buildinfo.Version=${VERSION} \
      -X github.com/glennprays/catet-backend/pkg/buildinfo.Commit=${COMMIT} \
      -X github.com/glennprays/catet-backend/pkg/buildinfo.BuildTime=${BUILD_TIME}" \
    -o /app/main ./cmd/api/main.go

# The operator CLI ships in the same image as the API: account
# break-glass work needs the same config and database. The final stage
# is scratch (no shell), so it runs as its own process:
#   docker compose exec api /catet user list
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" -o /app/catet ./cmd/catet

# Stage 2: Prepare CA certificates and timezone data
FROM debian:bullseye-slim AS certs-and-tzdata

# Install ca-certificates and tzdata
RUN apt-get update && apt-get install -y ca-certificates tzdata

# Stage 3: Run the Go application using scratch
FROM scratch

# Copy the compiled Go binary from the build stage
COPY --from=builder /app/main /main

# Copy the operator CLI; see the builder stage for how it is invoked.
COPY --from=builder /app/catet /catet

# Copy CA certificates from the certs stage

COPY --from=certs-and-tzdata /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# Copy timezone data from the certs-and-tzdata stage
COPY --from=certs-and-tzdata /usr/share/zoneinfo /usr/share/zoneinfo

COPY --from=certs-and-tzdata /etc/localtime /etc/localtime
COPY --from=certs-and-tzdata /etc/timezone /etc/timezone


# Set the working directory
WORKDIR /

# The API and CLI need no write access and no privileges; the scratch
# image has no /etc/passwd, so the uid is numeric.
USER 65532:65532

# Documents the API port (APP_PORT default 3000).
EXPOSE 3000


# Set the default timezone to UTC
ENV TZ=UTC

# Command to run the Go application
CMD ["/main"]

