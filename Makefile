SHELL := /bin/bash
.DEFAULT_GOAL := help

GO_TEST_ENV ?= GOCACHE=/tmp/borq-go-build GOMODCACHE=/tmp/borq-go-mod
BORQ_GO = python3 "$(CURDIR)/zarf/scripts/with-ui-lock.py" go

.PHONY: help
help: ## Show available commands.
	@awk 'BEGIN { FS = ":.*## "; print "Usage: make <command>\n" } /^[a-zA-Z0-9_-]+:.*## / { printf "  %-28s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

include zarf/make/dev.mk
include zarf/make/test.mk
include zarf/make/build.mk
