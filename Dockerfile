FROM golang:1.26-alpine AS build
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY src/frontend ./src/frontend
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /maildrop ./cmd/maildrop

FROM alpine:3.23
WORKDIR /app
RUN mkdir -p /app/data && chmod 700 /app/data
COPY --from=build /maildrop /usr/local/bin/maildrop
ENV DATABASE_PATH=/app/data/mailbox.db
EXPOSE 25 5000
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 CMD ["maildrop", "-healthcheck"]
ENTRYPOINT ["maildrop"]
