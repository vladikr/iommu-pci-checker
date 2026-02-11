FROM --platform=linux/arm64 public.ecr.aws/docker/library/golang:alpine AS builder

# 1. Install GCC for CGO
RUN apk add --no-cache build-base

# 2. Enable CGO (required for your assembly/C code)
ENV GOARCH=arm64
ENV CGO_ENABLED=1

# 3. ENABLE Modules (Remove 'off' or explicitly set 'on')
ENV GO111MODULE=on

WORKDIR /app
COPY main.go .

# 4. Initialize a module and download the missing dependency
RUN go mod init gh_check
RUN go mod tidy

# 5. Build
RUN go build -o gh_check

# Runtime Stage
FROM public.ecr.aws/docker/library/alpine:3.18
COPY --from=builder /app/gh_check /usr/bin/gh_check
ENTRYPOINT ["/usr/bin/gh_check"]
