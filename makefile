VERSION := $(shell git describe --tags --abbrev=0 2>/dev/null || echo "v0.1.0")
NEXT_VERSION := $(shell echo $(VERSION) | awk -F. '{print $$1"."$$2"."($$3 + 1)}')
REMOTE_REPO := $(shell git remote get-url origin)

publish:
	@git add .
	@git commit -m "Release $(NEXT_VERSION)" || echo "Nothing to commit."
	@git tag -a $(NEXT_VERSION) -m "Release $(NEXT_VERSION)"
	@git push $(REMOTE_REPO) main
	@git push $(REMOTE_REPO) --tags
	@echo "Published $(NEXT_VERSION) to $(REMOTE_REPO)"
	@echo $(NEXT_VERSION) > version.txt

.PHONY: publish


install:
	go install github.com/suisseworks/whagonsRTE@latest

install-skip-checksum:
	GOSUMDB=off go install github.com/suisseworks/whagonsRTE@latest


run:
	air
	