.PHONY: default bootstrap setup test lint fmt fmt-check check docker hooks build clean try

TESTDIR := $(HOME)/Documents/_SYSADMIN/cyberdyne

# Standard targets are thin shims; the implementations live in script/
# per the scripts-to-rule-them-all pattern.

default: check build

bootstrap:
	@script/bootstrap

setup:
	@script/setup

test:
	@script/test

lint:
	@script/lint

fmt:
	@script/fmt

fmt-check:
	@script/fmt-check

check:
	@script/check

docker:
	@script/docker

hooks:
	@script/install-precommit

build: clean
	@go build .

clean:
	@rm -f attrsum

try: build
	./attrsum sum add -v $(TESTDIR)
	./attrsum check -v $(TESTDIR)
	./attrsum clear -v $(TESTDIR)
	-./attrsum check -v $(TESTDIR)
	./attrsum sum add -v $(TESTDIR)
	./attrsum check -v $(TESTDIR)
	touch $(TESTDIR)/*
	./attrsum sum update -v $(TESTDIR)
	./attrsum check -v $(TESTDIR)
