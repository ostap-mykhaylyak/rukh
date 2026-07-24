APP      := rukh
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)

# Installation layout (override on the command line if needed).
SBIN_DIR := /usr/sbin
CONF_DIR := /etc/$(APP)
LOG_DIR  := /var/log/$(APP)
UNIT_DIR := /etc/systemd/system

.PHONY: all static build test vet fmt clean dirs install uninstall

all: static

build:
	go build -o bin/$(APP) ./cmd/$(APP)

static:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(APP) ./cmd/$(APP)

test:
	go test ./... -race -count=1

vet:
	go vet ./...

fmt:
	gofmt -s -w .

clean:
	rm -rf bin

dirs:
	install -d -m 0750 -o root -g root $(CONF_DIR)
	install -d -m 0750 -o root -g root $(LOG_DIR)

install: static dirs
	install -m 0755 bin/$(APP) $(SBIN_DIR)/$(APP)
	test -f $(CONF_DIR)/config.yaml || install -m 0640 internal/bootstrap/skel/etc/$(APP)/config.yaml $(CONF_DIR)/config.yaml
	install -m 0644 internal/bootstrap/$(APP).service $(UNIT_DIR)/$(APP).service
	install -m 0644 internal/bootstrap/skel/etc/logrotate.d/$(APP) /etc/logrotate.d/$(APP)
	@echo ""
	@echo "$(APP) $(VERSION) installed. Next steps:"
	@echo "  1. move nginx to a loopback port (listen 127.0.0.1:8080;) and reload it"
	@echo "  2. review $(CONF_DIR)/config.yaml"
	@echo "  3. systemctl daemon-reload"
	@echo "  4. systemctl enable --now $(APP)"
	@echo "  5. $(APP) status"

uninstall:
	-systemctl disable --now $(APP) 2>/dev/null
	rm -f $(SBIN_DIR)/$(APP) $(UNIT_DIR)/$(APP).service /etc/logrotate.d/$(APP)
	@echo "config and logs left in place ($(CONF_DIR), $(LOG_DIR)); remove manually or with '$(APP) --purge'"
