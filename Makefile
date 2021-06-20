.PHONY: help config

current_dir ?= $(shell pwd)

help: ## Display this help screen (default)
	@grep -h -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-20s\033[0m %s\n", $$1, $$2}'

build-binary:
	./build.sh

config: 
	cp ./example-config.yaml ./config.yaml

registration: build-binary
	./pulsesms -g -r registraion.yaml

build-docker:
	docker build -t pulsesms .




