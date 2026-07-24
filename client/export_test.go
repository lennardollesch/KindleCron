package client

// Test-only options that inject the runner and daemon starter, so the Client
// can be exercised without a real kron binary or spawning processes. They live
// in a _test.go file to keep them out of the public API.

func withRunner(fakeRun runner) Option {
	return func(cfg *config) { cfg.run = fakeRun }
}

func withStarter(fakeStart daemonStarter) Option {
	return func(cfg *config) { cfg.start = fakeStart }
}
