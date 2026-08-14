# syntax=docker/dockerfile:1

# 1. Frontend. web/src/protocol.ts and messages.ts are generated and committed.
FROM node:22-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# 2. Server, with the built frontend embedded.
FROM golang:1.27-bookworm AS server
WORKDIR /src
COPY go.mod go.sum ./
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /qbuzzer ./cmd/server

# 3. Runtime: a static binary on distroless, as a non-root user.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=server /qbuzzer /qbuzzer
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/qbuzzer"]
