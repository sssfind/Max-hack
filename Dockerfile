FROM golang:1.26-alpine AS builder

ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

WORKDIR /app

# Сначала копируем только файлы зависимостей для кэширования слоев Docker
COPY go.mod go.sum ./
RUN go mod download

# Копируем остальной исходный код
COPY . .

RUN go build -ldflags="-w -s" -o max-bot ./cmd/bot/main.go
RUN go build -ldflags="-w -s" -o migrator ./cmd/migrator/main.go

FROM alpine:3.19

WORKDIR /app

RUN apk --no-cache add ca-certificates wget tzdata

RUN wget -qO /usr/local/share/ca-certificates/russian_trusted_root_ca.crt https://gu-st.ru/content/lending/russian_trusted_root_ca_pem.crt && \
    wget -qO /usr/local/share/ca-certificates/russian_trusted_sub_ca.crt https://gu-st.ru/content/lending/russian_trusted_sub_ca_pem.crt && \
    update-ca-certificates

COPY --from=builder /app/max-bot .
COPY --from=builder /app/migrator .
COPY --from=builder /app/spo_program_vacancy_map.json .

RUN chmod +x ./max-bot ./migrator

CMD ["./max-bot"]