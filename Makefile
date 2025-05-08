TESTDIR := $(HOME)/Documents/_SYSADMIN/cyberdyne

default: test

test:
	@go test ./... -v

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

