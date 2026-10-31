# syntax=docker/dockerfile:1.7

FROM golang:1.27.3-alpine3.24@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS builder

ARG TARGETOS=linux
ARG TARGETARCH

ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download && go mod verify

# В сборку не попадают локальные секреты, документы и .git.
COPY cmd ./cmd
COPY internal ./internal
COPY spo_program_vacancy_map.json ./

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -mod=readonly -ldflags="-s -w -buildid=" -o /out/max-bot ./cmd/bot && \
    GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -mod=readonly -ldflags="-s -w -buildid=" -o /out/migrator ./cmd/migrator

FROM alpine:3.24.3@sha256:be2656061b536c1883c4034790e21d0a09c5aaaf51968b89368a3c0371ea2c9a

LABEL org.opencontainers.image.title="SkillGap" \
      org.opencontainers.image.description="MAX bot for transparent vocational-program and labour-market analysis"

ARG RUSSIAN_ROOT_CA_SHA256=936a43fea6e8e525bcc0f81acd9c3d21b4fc4b9b68acea7906d698005afc6504
ARG RUSSIAN_SUB_CA_SHA256=f0ae589f36774f29ef3648f7984b08d42fcce6f1ffeeb6236d773daeb2744ea6

RUN apk --no-cache add ca-certificates tzdata wget && \
    wget -qO /usr/local/share/ca-certificates/russian_trusted_root_ca.crt \
      https://gu-st.ru/content/lending/russian_trusted_root_ca_pem.crt && \
    wget -qO /usr/local/share/ca-certificates/russian_trusted_sub_ca.crt \
      https://gu-st.ru/content/lending/russian_trusted_sub_ca_pem.crt && \
    echo "${RUSSIAN_ROOT_CA_SHA256}  /usr/local/share/ca-certificates/russian_trusted_root_ca.crt" | sha256sum -c - && \
    echo "${RUSSIAN_SUB_CA_SHA256}  /usr/local/share/ca-certificates/russian_trusted_sub_ca.crt" | sha256sum -c - && \
    update-ca-certificates && \
    addgroup -S -g 10001 skillgap && \
    adduser -S -D -H -u 10001 -G skillgap skillgap

WORKDIR /app

COPY --from=builder --chown=10001:10001 /out/max-bot /out/migrator ./
COPY --from=builder --chown=10001:10001 /src/spo_program_vacancy_map.json ./
COPY --chown=10001:10001 THIRD_PARTY_NOTICES ./THIRD_PARTY_NOTICES

USER 10001:10001

EXPOSE 8080

CMD ["./max-bot"]
