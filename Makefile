.PHONY: default bootstrap setup test lint fmt fmt-check check docker hooks build clean try install

TESTDIR := $(HOME)/Documents/_SYSADMIN/cyberdyne
PREFIX ?= /usr/local

# Standard targets are thin shims; the implementations live in script/
# per the scripts-to-rule-them-all pattern.

default: ./attrsum

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

./attrsum: *.go go.*
	@go build .

build: ./attrsum

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

install: ./attrsum
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 0755 ./attrsum $(DESTDIR)$(PREFIX)/bin/attrsum
