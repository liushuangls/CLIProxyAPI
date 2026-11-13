FROM node:24-bookworm-slim AS panel

WORKDIR /panel
RUN npm install --global bun@1.4.3
COPY management-ui/package.json management-ui/bun.lock ./
RUN bun install --frozen-lockfile
COPY management-ui/ ./
ARG VERSION=self
RUN VERSION="${VERSION}" bun run build

FROM golang:1.26-bookworm AS builder

WORKDIR /app
RUN apt-get update && apt-get install -y --no-install-recommends build-essential git && rm -rf /var/lib/apt/lists/*
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=self
ARG COMMIT=none
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=1 GOOS=linux go build -buildvcs=false -ldflags="-s -w -X 'main.Version=${VERSION}' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o /CLIProxyAPI ./cmd/server/

FROM debian:bookworm

RUN apt-get update && apt-get install -y --no-install-recommends tzdata ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /CLIProxyAPI
COPY --from=builder /CLIProxyAPI ./CLIProxyAPI
COPY --from=panel /panel/dist/index.html ./static/management.html
COPY config.example.yaml ./config.example.yaml

ENV TZ=Asia/Shanghai \
    MANAGEMENT_STATIC_PATH=/CLIProxyAPI/static \
    MANAGEMENT_PANEL_BUNDLED=true
RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone
EXPOSE 8317
CMD ["./CLIProxyAPI"]
