GO      := go
GOFLAGS := -tags netgo,usergo -v -buildvcs
DESTDIR := _output/bin
TOOLS_BASE := requestor quotable isolated runkube
TOOLS    := $(addprefix $(DESTDIR)/,$(TOOLS_BASE))

all: $(TOOLS)

$(TOOLS): $(DESTDIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $@ ./tools/$(notdir $@)

$(DESTDIR):
	@mkdir -p $(DESTDIR)

run: $(DESTDIR)/runkube
	@./$(DESTDIR)/runkube --log-level info

test:
	@CGO_ENABLED=0 go test -v ./...

# Build the binary once, then run the end-to-end tests against it. These boot a
# real single-node cluster and therefore need a working Linux sandbox (user
# namespaces); the tests skip when the sandbox is unavailable. RUNKUBE_ITEST_BASE
# overrides the parent directory for the throwaway cluster root (default:
# $HOME/.cache/runkube-itest; it must not be /tmp).
test-integration: $(DESTDIR)/runkube
	@RUNKUBE_BIN=$(CURDIR)/$(DESTDIR)/runkube CGO_ENABLED=0 go test -count=1 -v ./tests/

clean:
	@rm -v -r -f $(DESTDIR)

.PHONY: all run clean test-integration $(TOOLS)
