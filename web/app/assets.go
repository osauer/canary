package appweb

import (
	"embed"
	"path/filepath"
)

// Files contains the read-only static PWA assets served by the app host. It is
// presentation content only and carries no runtime state or authority.
//
//go:embed index.html app.js alert-inbox.js alerts.js auth.js brief.js chrome.js earnings-relevance.js exposure-relevance.js lifecycle.js market-events.js opportunities.js orders.js portfolio.js protection-coverage.js protection.js render-runtime.js rules-presentation.js settings.js shared.js shell.js state.js strategies.js stress.js underlyings.js update.js styles.css service-worker.js manifest.webmanifest icon-192.png icon-512.png favicon-16.png favicon-32.png favicon-64.png
var Files embed.FS

// EmbeddedJavaScriptFileNames returns a newly allocated, filename-sorted list
// of root-level JavaScript assets in Files. The embedded set is shared by local
// static routes and the relay path allowlist. The function panics only if the
// compile-time embedded filesystem cannot read its root.
func EmbeddedJavaScriptFileNames() []string {
	entries, err := Files.ReadDir(".")
	if err != nil {
		panic(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".js" {
			names = append(names, entry.Name())
		}
	}
	return names
}
