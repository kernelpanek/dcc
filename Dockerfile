FROM golang:latest AS BUILD
RUN mkdir /go/src/app
ADD . /go/src/app/
WORKDIR /go/src/app
RUN curl https://glide.sh/get | sh
RUN glide update && glide install --strip-vendor
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o main .

FROM scratch
COPY --from=BUILD /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=BUILD /go/src/app/main  /
COPY --from=BUILD /go/src/app/config.yaml /config/
CMD ["/main"]