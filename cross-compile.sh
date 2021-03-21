
#!/bin/bash

rm -rf bin/*
mkdir -p bin

# linux
export GOOS=linux
export GOARCH=amd64
mkdir bin/$GOOS-$GOARCH
go build -v -o bin/$GOOS-$GOARCH/themo-prometheus

# raspberry 4
export GOOS=linux
export GOARCH=arm64
mkdir bin/$GOOS-$GOARCH
go build -v -o bin/$GOOS-$GOARCH/themo-prometheus

# windows
export GOOS=windows
export GOARCH=amd64
mkdir bin/$GOOS-$GOARCH
go build -v -o bin/$GOOS-$GOARCH/themo-prometheus.exe

cp config.sample.yaml bin/
