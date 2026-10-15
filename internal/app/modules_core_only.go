//go:build core_only

package app

// The core-only build: no optional module is imported, so none is
// compiled in. Nav has no module pages, their routes answer 404 and
// the sitemap leaves them out. CI builds this configuration (and runs
// TestCoreOnly*) so that adding a module can never make the core
// depend on it.
