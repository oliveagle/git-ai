# The two suites a project of this size has, and the one command the policy
# runs. The CI calls `make test`; an agent calls `make test-unit` before it asks
# for a land.
GO ?= go

.PHONY: test test-unit test-integration

# Everything the checkpoint covers. The verifier and the merge queue run this.
test: test-unit test-integration

# Seconds, no service: safe to run in the workspace of the agent.
test-unit:
	$(GO) test -count=1 ./internal/... ./cmd/...

# Minutes, a real process and a real socket: run on the whole tree of the
# result, never on a sparse one.
test-integration:
	$(GO) test -count=1 -tags=integration -timeout=5m ./test/integration/...
