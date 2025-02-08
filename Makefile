GO      := go
GOFLAGS := -tags netgo,usergo -v -buildvcs

DESTDIR := _output/bin

TOOLS_BASE := requestor quotable isolated runkube
TOOLS    := $(addprefix $(DESTDIR)/,$(TOOLS_BASE))

all: $(TOOLS)

all: $(TOOLS)

$(TOOLS): $(DESTDIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $@ ./tools/$(notdir $@)

$(DESTDIR):
	@mkdir -p $(DESTDIR)

run: $(DESTDIR)/runkube
	@./$(DESTDIR)/runkube

clean:
	@rm -v -r -f $(DESTDIR)

.PHONY: all run clean $(TOOLS)
