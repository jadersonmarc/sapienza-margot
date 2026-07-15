# sapienza-margot — data plane WhatsApp (Go). Build é hermético via vendor/
# (o sapienza-kit é um replace local, então fica vendorizado no repo). Sobe
# independente do core no Coolify; conecta ao MESMO Postgres via DATABASE_URL.
FROM golang:1.26-bookworm AS build
WORKDIR /app
COPY . .
# -mod=vendor: sem rede, usa vendor/ (inclui o sapienza-kit).
RUN CGO_ENABLED=0 go build -mod=vendor -trimpath -o /out/margot ./cmd/server

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/margot /margot
EXPOSE 8081
ENTRYPOINT ["/margot"]
