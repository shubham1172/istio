#!/bin/bash

# This script is used to build the pilot image and push it to the registry
export GOPATH=$HOME/go
export GOCACHE=/tmp/.cache/go-build
export HUB=ghcr.io/shubham1172
export TAG=dev
export ISTIO=$GOPATH/src/istio/istio

make build