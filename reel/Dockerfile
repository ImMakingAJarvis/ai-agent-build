FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/reeld ./cmd/reeld

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/reeld /reeld
EXPOSE 8122
ENTRYPOINT ["/reeld"]
