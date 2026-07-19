FROM golang:1.26.5 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /bot ./cmd/bot

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /bot /bot
EXPOSE 8080 8081
ENTRYPOINT ["/bot"]
CMD ["serve"]
