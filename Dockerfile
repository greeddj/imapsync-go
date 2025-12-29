# syntax=docker/dockerfile:1
FROM gcr.io/distroless/static-debian13:nonroot
WORKDIR /
COPY ./dist/imapsync-go /imapsync-go
ENTRYPOINT [ "/imapsync-go" ]
