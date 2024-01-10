#!/bin/bash
docker build --pull --rm --build-arg BASE_CONTAINER=ubuntu:22.04 -f Dockerfile -t xdevlab/controller:$1 "../.."
