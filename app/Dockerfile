# syntax=docker/dockerfile:1.17

# Build stage for frontend
FROM node:22-alpine AS frontend-builder

WORKDIR /app/web

COPY web/package.json web/pnpm-lock.yaml* web/pnpm-workspace.yaml ./

# Pin pnpm: `pnpm@latest` changes build-script behavior between majors (v11
# removed onlyBuiltDependencies), which silently breaks reproducible builds.
RUN npm install -g pnpm@11.5.1 && pnpm install --frozen-lockfile

COPY web/ .
RUN pnpm run build

# Build stage for backend
FROM golang:1.26-alpine AS backend-builder

WORKDIR /app

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o mqtt-mail .

# Final stage
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=backend-builder /app/mqtt-mail /mqtt-mail
COPY --from=frontend-builder /app/web/dist /web/dist

USER nonroot:nonroot

ENTRYPOINT ["/mqtt-mail", "/var/lib/mqtt-mail/config.json"]
