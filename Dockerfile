FROM golang:1.26-alpine AS builder

# Отключаем CGO для статической линковки бинарника (чтобы он запустился в чистом alpine)
ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

WORKDIR /app

# Сначала копируем только файлы зависимостей для кэширования слоев Docker
COPY go.mod go.sum ./
RUN go mod download

# Копируем остальной исходный код
COPY deploy/docker/core .

RUN go build -ldflags="-w -s" -o max-bot ./bot/cmd/main.go
RUN go build -ldflags="-w -s" -o migrator ./bot/cmd/migrator/main.go

FROM alpine:3.19

WORKDIR /app

RUN apk --no-cache add ca-certificates wget tzdata

RUN wget -qO /usr/local/share/ca-certificates/russian_trusted_root_ca.crt https://gu-st.ru/content/lending/russian_trusted_root_ca_pem.crt && \
    wget -qO /usr/local/share/ca-certificates/russian_trusted_sub_ca.crt https://gu-st.ru/content/lending/russian_trusted_sub_ca_pem.crt && \
    update-ca-certificates

COPY --from=builder /app/max-bot .

WORKDIR /app

COPY --from=builder /app/max-bot .
COPY --from=builder /app/migrator .

RUN chmod +x ./max-bot

CMD ["./max-bot"]