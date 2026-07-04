# Image configuration
IMAGE_REGISTRY ?= cfall
IMAGE_NAME ?= gpu-fallback-webhook
IMAGE_TAG ?= latest
IMAGE ?= $(IMAGE_REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)

.PHONY: all test build docker-build certs deploy undeploy clean

all: test build

## test: Run unit tests
test:
	go test -v ./...

## build: Build the webhook binary locally
build:
	CGO_ENABLED=0 go build -o bin/gpu-fallback-webhook main.go

## docker-build: Build the docker image for the webhook
docker-build:
	docker build -t $(IMAGE) .

## certs: Generate TLS certificates and patch MutatingWebhookConfiguration
certs:
	./deploy/generate-certs.sh

## deploy: Generate certs and deploy all components to Kubernetes
deploy: certs
	kubectl apply -f deploy/deployment.yaml
	kubectl apply -f deploy/webhook-configuration-active.yaml

## undeploy: Remove all components from Kubernetes
undeploy:
	kubectl delete -f deploy/webhook-configuration-active.yaml --ignore-not-found
	kubectl delete -f deploy/deployment.yaml --ignore-not-found

## clean: Clean build binaries and active configurations
clean:
	rm -rf bin/
	rm -f deploy/webhook-configuration-active.yaml
