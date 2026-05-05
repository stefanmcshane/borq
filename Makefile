.PHONY: test test-integration

test:
	go test ./...

test-integration:
	go test -tags=integration ./business/managementbootstrap -run TestExecuteIntegration -v -count=1
