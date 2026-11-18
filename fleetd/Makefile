# fleetd — build/test/deploy surface (agents + operator; every target ends
# with an unambiguous PASS/FAIL line).
SHA     := $(shell git rev-parse --short HEAD)
LDFLAGS := -ldflags "-X main.version=$(SHA)"
HOST    := host-01
SSH     := ssh -i ~/.ssh/example_host -o IdentitiesOnly=yes
DEST    := ~/fleetd

.PHONY: build vet smoke test deploy rollback status stop start logs chain

build:
	@go build $(LDFLAGS) -o fleetd . && echo "BUILD PASS ($(SHA))" || (echo "BUILD FAIL"; exit 1)

vet:
	@go vet ./... && echo "VET PASS" || (echo "VET FAIL"; exit 1)

test:
	@go test ./... && echo "TEST PASS" || (echo "TEST FAIL"; exit 1)

smoke: build
	@./deploy/smoke.sh && echo "SMOKE PASS" || (echo "SMOKE FAIL"; exit 1)

# fleetd runs as the SYSTEM LaunchDaemon system/com.example.fleetd (since
# 567bd21) — never a LaunchAgent: a LaunchAgent copy would run a SECOND
# supervisor next to it. The plist is installed by infra/host-setup.sh; these
# targets only swap the binary and restart the daemon.
LABEL   := system/com.example.fleetd
PLIST   := /Library/LaunchDaemons/com.example.fleetd.plist

# deploy: keep the previous binary for a 30-second rollback; swap atomically
# (mv, never overwrite a running binary in place); restart via kickstart.
# The live fleetd.json is NOT overwritten — a drift from deploy/fleetd.json is
# printed so the operator decides. The host is an INTEL Mac (x86_64) —
# cross-compile explicitly; a native arm64 build is "Bad CPU type" there.
deploy: vet test
	@GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o fleetd-amd64 . && echo "BUILD PASS amd64 ($(SHA))" || (echo "BUILD FAIL"; exit 1)
	@scp -i ~/.ssh/example_host -o IdentitiesOnly=yes fleetd-amd64 $(HOST):$(DEST)/fleetd.new
	@scp -i ~/.ssh/example_host -o IdentitiesOnly=yes deploy/fleetd.json $(HOST):$(DEST)/fleetd.json.repo
	@$(SSH) $(HOST) 'cd $(DEST) && python3 -c "import json,sys; a,b=(json.load(open(f)) for f in sys.argv[1:]); sys.exit(a!=b)" fleetd.json fleetd.json.repo \
		|| echo "!! live fleetd.json differs from the repo deploy/fleetd.json — not overwritten; review: diff $(DEST)/fleetd.json $(DEST)/fleetd.json.repo"'
	@$(SSH) $(HOST) 'cd $(DEST) && chmod +x fleetd.new && cp fleetd fleetd.prev && mv fleetd.new fleetd && sudo -n launchctl kickstart -k $(LABEL)' \
		&& echo "DEPLOY PASS ($(SHA) → $(HOST); previous binary kept as fleetd.prev)" || (echo "DEPLOY FAIL"; exit 1)

rollback:
	@$(SSH) $(HOST) 'cd $(DEST) && cp fleetd.prev fleetd.rollback && mv fleetd.rollback fleetd && sudo -n launchctl kickstart -k $(LABEL)' \
		&& echo "ROLLBACK PASS" || (echo "ROLLBACK FAIL"; exit 1)

status:
	@$(SSH) $(HOST) 'sudo -n launchctl print $(LABEL) | grep -E "^\s+(state|pid) =" && tail -5 $(DEST)/fleetd.log' \
		&& echo "STATUS PASS" || (echo "STATUS FAIL (not loaded?)"; exit 1)

# stop: STOP flags first (so a restart skips cycles), then bootout (KeepAlive
# would respawn a merely killed daemon). Flag paths match deploy/fleetd.json.
stop:
	@$(SSH) $(HOST) 'touch ~/signal-fleet/STOP ~/bugfix-fleet-STOP; sudo -n launchctl bootout $(LABEL)' \
		&& echo "STOP PASS (flags set + daemon booted out)" || (echo "STOP FAIL"; exit 1)

start:
	@$(SSH) $(HOST) 'rm -f ~/signal-fleet/STOP ~/bugfix-fleet-STOP; sudo -n launchctl bootstrap system $(PLIST)' \
		&& echo "START PASS" || (echo "START FAIL"; exit 1)

logs:
	@$(SSH) $(HOST) 'tail -40 $(DEST)/fleetd.log'

chain: build
	@./fleetd chain -config deploy/chain.json $(if $(ISSUE),-issue $(ISSUE),) && echo "CHAIN PASS" || (echo "CHAIN FAIL"; exit 1)
