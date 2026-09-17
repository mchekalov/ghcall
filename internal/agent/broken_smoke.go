package agent

// Deliberately broken for a local ci-tools ci-autofix end-to-end test: this
// references an undefined symbol so `go build ./...` fails in CI. Safe to
// delete once the test is done.
var ciAutofixSmokeTest = undefinedSmokeTestSymbol
