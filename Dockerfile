FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

COPY go.mod ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=0.1.0" -o my-vinyl-api .

# ---- runtime ----
FROM alpine:3.24

RUN addgroup -S app && adduser -S app -G app

WORKDIR /app

COPY --from=builder /app/my-vinyl-api .

# Numeric, not the name - the kubelet cannot verify runAsNonRoot for a user it
# cannot resolve, and fails the container closed. Same uid the account already has.
USER 100

EXPOSE 8080

ENTRYPOINT ["./my-vinyl-api"]
