PREFIX ?= /usr/local
BINARY := cfdyndns

.PHONY: build install uninstall clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) .

install:
	@test -f $(BINARY) || { echo "run 'make' first (as your user)"; exit 1; }
	install -Dm755 $(BINARY) $(PREFIX)/bin/$(BINARY)
	install -Dm644 cfdyndns.service /etc/systemd/system/cfdyndns.service
	@if [ ! -f /etc/cfdyndns/cfdyndns.env ]; then \
		install -Dm600 cfdyndns.env.example /etc/cfdyndns/cfdyndns.env; \
		echo "Created /etc/cfdyndns/cfdyndns.env - edit it, then run:"; \
		echo "  systemctl enable --now cfdyndns"; \
	fi
	systemctl daemon-reload

uninstall:
	-systemctl disable --now cfdyndns
	rm -f $(PREFIX)/bin/$(BINARY) /etc/systemd/system/cfdyndns.service
	systemctl daemon-reload
	@echo "Left /etc/cfdyndns in place; remove it manually if desired."

clean:
	rm -f $(BINARY)
