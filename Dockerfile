ARG virter_image=virter

FROM golang:alpine as builder

WORKDIR /go/src/vmshed
COPY . .

RUN go build && mv ./vmshed /

FROM ${virter_image}

COPY --from=builder /vmshed /opt/virter/
ENV PATH="/opt/virter:${PATH}"

WORKDIR /opt/virter
CMD ["-h"]
ENTRYPOINT ["/opt/virter/vmshed"]
